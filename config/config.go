package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultRelConfigDir = ".config/easyshift"
	DefaultLogFile      = "easyshift.log"
	DefaultPullSecret   = "pull-secret"
	DefaultWebPort      = 9393
	DefaultClustersMax  = 3
	DefaultWorkersMax   = 3

	// BaseNetworkRange is the /24 prefix for the shared NAT network. It
	// deliberately avoids the common home-LAN ranges (192.168.0/1.x) so the
	// virtual network doesn't collide with the host's real LAN.
	BaseNetworkRange = "192.168.126"
	NetworkStart     = 5
	NetworkEnd       = 20

	// DHCPDynamicStart/DHCPDynamicEnd bound the *dynamic* DHCP pool on the
	// shared NAT network. The pool is kept strictly disjoint from — and above —
	// the static reservation band [NetworkStart, NetworkEnd] so a master can
	// never be handed a dynamic lease that collides with a reserved address (or
	// strays onto one before its reservation propagates to dnsmasq). Masters are
	// pinned statically anyway (see ClusterConfig.StaticNetworkKeyfile), so in
	// practice nothing should ever lease from this pool; it exists only as a
	// safety net.
	DHCPDynamicStart = 100
	DHCPDynamicEnd   = 254

	// SharedNATNetwork is the single libvirt network ALL NAT-mode clusters
	// attach to. It is a host-global resource (not per-cluster): created once
	// on the first NAT cluster and never torn down by deleting one cluster, so
	// clusters share one L2 segment and can reach each other — important for
	// disaster-recovery topologies (hub/spoke, replication, etc.). Per-cluster
	// addressing is kept distinct via GlobalState.UsedIPs/UsedMACs plus a DHCP
	// reservation per master added to this network.
	SharedNATNetwork = "easyshift-nat"

	// DefaultOCPVersion is a channel alias, not a pinned version. When set,
	// ClusterManager.Create resolves it to the actual current version by
	// fetching release.txt from the mirror. Users who want a specific
	// release pass --version 4.21.0 (or whatever) on the CLI.
	DefaultOCPVersion = OCPChannelStable
	// OCPChannelStable is the mirror alias for the latest stable z-stream.
	OCPChannelStable = "stable"
	OCPMirrorURL     = "https://mirror.openshift.com/pub/openshift-v4/x86_64"

	// DefaultStoragePool is libvirt's conventional auto-created pool name.
	// The pool name is just a label; on some hosts the pool at
	// /var/lib/libvirt/images is named differently (e.g. "images"), so this
	// is overridable per cluster via --storage-pool.
	DefaultStoragePool = "default"

	// NetworkModeNAT puts cluster VMs behind a libvirt NAT network. The host
	// will (eventually) run HAProxy + DNSmasq to expose the cluster.
	NetworkModeNAT = "nat"
	// NetworkModeBridge attaches each VM to an existing Linux bridge on the
	// host (e.g. br0) that is connected to the LAN. VMs get LAN IPs from the
	// upstream router and can reach the host. No HAProxy is required; the
	// user is responsible for the bridge, a DHCP reservation, and DNS records
	// pointing api.<name>.<domain> and *.apps.<name>.<domain> at the master.
	NetworkModeBridge = "bridge"

	// MagicDNS* select an IP-encoding wildcard DNS service so the cluster's
	// names resolve to the master IP with no DNS server of our own. The
	// values are string-valued (not a bool) so additional services slot in
	// without a new flag.
	//
	//   MagicDNSAuto  - NAT mode -> sslip.io; bridge mode -> off.
	//   MagicDNSSslip - force sslip.io (any mode), keyed on the master IP.
	//   MagicDNSNip   - force nip.io.
	//   MagicDNSOff   - disabled; use --base-domain + manual/--dns-provider.
	MagicDNSAuto  = "auto"
	MagicDNSSslip = "sslip.io"
	MagicDNSNip   = "nip.io"
	MagicDNSOff   = "off"

	// ClusterState* are the persisted values of ClusterConfig.State.
	ClusterStateNone     = "none"
	ClusterStateCreating = "creating"
	ClusterStateRunning  = "running"
	ClusterStateStopped  = "stopped"
	ClusterStateError    = "error"
)

// Config is the global on-disk configuration. There is no singleton; callers
// construct one via LoadConfig and pass it to managers. The pull secret is
// NOT stored here — see pullsecret.go for its dedicated on-disk file.
type Config struct {
	ConfigDir   string           `json:"configDir"`
	LogFile     string           `json:"logFile"`
	WebPort     int              `json:"webPort"`
	Debug       bool             `json:"debug"`
	Clusters    []*ClusterConfig `json:"clusters"`
	GlobalState *GlobalState     `json:"globalState"`
}

// ClusterConfig is the persisted spec for one cluster.
type ClusterConfig struct {
	Name         string `json:"name"`
	Domain       string `json:"domain"`
	OCPVersion   string `json:"ocpVersion"`
	MasterCount  int    `json:"masterCount"`
	WorkerCount  int    `json:"workerCount"`
	MasterRAM    int    `json:"masterRAM"`
	WorkerRAM    int    `json:"workerRAM"`
	MasterCPUs   int    `json:"masterCPUs"`
	WorkerCPUs   int    `json:"workerCPUs"`
	MasterDiskGB int    `json:"masterDiskGB"`
	WorkerDiskGB int    `json:"workerDiskGB"`

	// NetworkMode is "nat" or "bridge". See the corresponding constants.
	NetworkMode string `json:"networkMode"`
	// Bridge is the name of the existing host Linux bridge VMs attach to in
	// bridge mode (e.g. "br0"). Ignored when NetworkMode == "nat".
	Bridge string `json:"bridge,omitempty"`
	// MasterMAC is the MAC the user reserved at their router for the master
	// VM in bridge mode. Required in bridge mode; ignored in NAT mode.
	MasterMAC string `json:"masterMAC,omitempty"`
	// MasterIP is the IP the user's router will hand out to MasterMAC.
	// Required in bridge mode; ignored in NAT mode.
	MasterIP string `json:"masterIP,omitempty"`
	// MachineCIDR feeds networking.machineNetwork in install-config.yaml. In
	// NAT mode this is auto-derived from the libvirt subnet; in bridge mode
	// it is derived from MasterIP as /24 unless the user overrides it.
	MachineCIDR string `json:"machineCIDR,omitempty"`
	// Gateway is the bridge-mode default gateway baked into the master's
	// static network config. Defaults to the .1 host of MachineCIDR; ignored
	// in NAT mode. See StaticNetworkKeyfile.
	Gateway string `json:"gateway,omitempty"`
	// DNS is a comma-separated list of DNS servers for the master's static
	// network config in bridge mode. Defaults to Gateway; ignored in NAT mode.
	DNS string `json:"dns,omitempty"`
	// StoragePool is the libvirt pool where the master disk and boot ISO are
	// created. Defaults to "default"; override with --storage-pool when your
	// host's pool has a different name.
	StoragePool string `json:"storagePool,omitempty"`
	// DNSProvider, if set, names the public DNS backend easyshift will use
	// to create the cluster's A records (api, api-int, *.apps). When
	// empty, DNS records are the user's responsibility — the preflight
	// still verifies they resolve correctly. Persisted so Delete picks the
	// right backend when tearing down a cluster.
	DNSProvider string `json:"dnsProvider,omitempty"`
	// DNSZone is the parent DNS zone that owns the cluster's records. When
	// empty, defaults to Domain (the base domain). Override when the zone
	// is a parent of the base domain, e.g. Domain="dev.rtalur.dev" but the
	// Cloudflare zone is "rtalur.dev".
	DNSZone string `json:"dnsZone,omitempty"`
	// TLSEmail, when non-empty, enables Let's Encrypt cert issuance for
	// api.<fqdn> and *.apps.<fqdn> via ACME DNS-01. Requires DNSProvider
	// to be set (the same provider+token are reused for the challenge).
	// The email is the ACME account email.
	TLSEmail string `json:"tlsEmail,omitempty"`
	// TLSStaging, when true, points ACME at Let's Encrypt's staging
	// endpoint. Useful while iterating because staging has much higher
	// rate limits, but issues certs signed by an untrusted root.
	TLSStaging bool `json:"tlsStaging,omitempty"`
	// MagicDNS, when set to a wildcard service (sslip.io / nip.io), makes
	// easyshift derive Domain as "<masterIP>.<service>" so every cluster
	// name resolves to the master IP with no DNS records to manage. Empty
	// means off (use Domain + manual/provider DNS). Resolved from the
	// --magic-dns flag's "auto"/"off" by the manager before any stage runs.
	MagicDNS string `json:"magicDNS,omitempty"`

	NetworkSubnet string   `json:"networkSubnet"`
	IPAddresses   []string `json:"ipAddresses"`
	MACAddresses  []string `json:"macAddresses"`
	State         string   `json:"state"`

	// BootISOVolPath is the libvirt default-pool path of the master's
	// bootstrap-in-place ISO, set by the embed-ignition-iso stage and used
	// by create-master-vms. Persisted so a resumed build reuses it.
	BootISOVolPath string `json:"bootISOVolPath,omitempty"`
	// InstallKernelCmdline is the macOS install-phase kernel command line
	// (ignition.config.url + rootfs url + …) computed by publish-pxe-assets and
	// used by create-master-vms for the vfkit linux bootloader. macOS only.
	InstallKernelCmdline string `json:"installKernelCmdline,omitempty"`
	// KubeconfigTarget is the user kubeconfig file the merge-kubeconfig stage
	// wrote the cluster's context into (resolved from $KUBECONFIG at create
	// time), so delete cleans the same file even if the env changed since.
	KubeconfigTarget string `json:"kubeconfigTarget,omitempty"`
}

// GlobalState tracks resources allocated across all clusters on this host.
type GlobalState struct {
	UsedIPs       map[string]bool `json:"usedIPs"`
	UsedMACs      map[string]bool `json:"usedMACs"`
	ActiveCluster string          `json:"activeCluster"`
}

// DefaultConfigDir returns the absolute path of the default config dir
// (~/.config/easyshift, or a temp fallback if HOME is unset).
func DefaultConfigDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, DefaultRelConfigDir)
	}
	return filepath.Join(os.TempDir(), "easyshift")
}

// NewDefaultConfig returns a Config populated with default paths and ports,
// rooted at configDir. It does not touch the filesystem.
func NewDefaultConfig(configDir string) *Config {
	return &Config{
		ConfigDir: configDir,
		LogFile:   filepath.Join(configDir, DefaultLogFile),
		WebPort:   DefaultWebPort,
		GlobalState: &GlobalState{
			UsedIPs:  map[string]bool{},
			UsedMACs: map[string]bool{},
		},
	}
}

// LoadConfig reads the config from configDir/config.json. If the file does
// not exist, a default config is written and returned.
func LoadConfig(configDir string) (*Config, error) {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	cfg := NewDefaultConfig(configDir)
	configFile := filepath.Join(configDir, "config.json")
	data, err := os.ReadFile(configFile)
	if errors.Is(err, os.ErrNotExist) {
		if err := cfg.Save(); err != nil {
			return nil, fmt.Errorf("write default config: %w", err)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.GlobalState == nil {
		cfg.GlobalState = &GlobalState{UsedIPs: map[string]bool{}, UsedMACs: map[string]bool{}}
	}
	if cfg.GlobalState.UsedIPs == nil {
		cfg.GlobalState.UsedIPs = map[string]bool{}
	}
	if cfg.GlobalState.UsedMACs == nil {
		cfg.GlobalState.UsedMACs = map[string]bool{}
	}
	return cfg, nil
}

// Save writes the config to configDir/config.json.
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	configFile := filepath.Join(c.ConfigDir, "config.json")
	if err := os.WriteFile(configFile, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

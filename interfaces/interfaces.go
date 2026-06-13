// Package interfaces holds every side-effect contract easyshift depends on,
// the value types passed across them, and the Stage/runner plumbing. It has
// no behavior and no constructors — providers implement these contracts,
// stages consume them, and app wires concretes in. It imports only config.
package interfaces

import (
	"context"
	"io"
	"time"

	"github.com/TheEasyShift/easyshift/config"
)

// CommandRunner abstracts process execution so business logic can be tested
// without invoking real binaries (virsh, virt-install, openshift-install, ...).
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunStreaming(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}

// Downloader abstracts HTTP downloads (OCP binaries, RHCOS images, ...).
type Downloader interface {
	Download(ctx context.Context, url, destPath string) error
}

// VMSpec describes a VM the VMManager should create.
type VMSpec struct {
	Name        string
	MemoryMiB   int
	VCPUs       int
	DiskSizeGiB int
	StoragePool string
	MAC         string
	// NetworkArg is passed verbatim to `virt-install --network`.
	NetworkArg string
	// BootISO, if non-empty, boots the VM from this ISO (a pool volume path).
	BootISO string
	// KernelArgs are PXE kernel args (used when BootISO is empty).
	KernelArgs string
}

// VMManager abstracts libvirt VM lifecycle plus the storage helpers needed to
// make boot media accessible to qemu:///system, and the cheap preflight
// probes (libvirt reachable, storage pool active) so stages never have to
// know virsh syntax.
type VMManager interface {
	Create(ctx context.Context, spec VMSpec) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	IsRunning(ctx context.Context, name string) (bool, error)
	// ImportISO uploads localPath into the named storage pool as volName and
	// returns the pool volume path for use as VMSpec.BootISO.
	ImportISO(ctx context.Context, pool, volName, localPath string) (string, error)
	// RemoveISO deletes a volume previously created by ImportISO.
	RemoveISO(ctx context.Context, pool, volName string) error
	// CheckAccess probes that the libvirt endpoint is reachable (libvirtd up,
	// user in the libvirt group, no polkit denial).
	CheckAccess(ctx context.Context) error
	// StoragePoolActive verifies the named pool exists and is running.
	StoragePoolActive(ctx context.Context, pool string) error
}

// NetworkSpec describes the shared libvirt NAT network. Bridge is normally
// empty so libvirt auto-assigns a virbrN (the bridge interface name is capped
// at 15 chars); Domain is empty under magic DNS so dnsmasq forwards the
// wildcard-service queries upstream.
type NetworkSpec struct {
	Name   string
	Bridge string
	Subnet string // e.g. "192.168.126"
	Domain string // libvirt DNS domain (empty = none)
}

// DHCPHost is a per-master reservation on the shared NAT network: it pins IP
// to MAC and hands the VM Hostname via DHCP option 12 (so RHCOS's
// node-valid-hostname is satisfied without an SSH hostname injector).
type DHCPHost struct {
	MAC      string
	IP       string
	Hostname string
}

// DHCPLease is a single dynamic lease observed on the network (as opposed to a
// static DHCPHost reservation). Used only for the nat-network reset report.
type DHCPLease struct {
	MAC      string
	IP       string
	Hostname string
}

// NetworkInfo is a read-only snapshot of the shared NAT network's live state,
// used by the nat-network reset command to detect drift between the network and
// easyshift's config. DHCPRangeStart/End are full IPs (e.g. "192.168.126.100"),
// empty when the network defines no DHCP range.
type NetworkInfo struct {
	Exists         bool
	DHCPRangeStart string
	DHCPRangeEnd   string
	Reservations   []DHCPHost
	Leases         []DHCPLease
}

// NetworkProvisioner manages the shared NAT network. EnsureNetwork creates it
// idempotently (it's a host-global resource shared by all NAT clusters);
// AddHost/RemoveHost add and remove a single master's DHCP reservation
// without disturbing the network or other clusters' reservations.
type NetworkProvisioner interface {
	EnsureNetwork(ctx context.Context, spec NetworkSpec) error
	AddHost(ctx context.Context, network string, host DHCPHost) error
	RemoveHost(ctx context.Context, network string, host DHCPHost) error
	// InspectNetwork reads the network's live definition + leases. A
	// non-existent network returns NetworkInfo{Exists: false} and no error.
	InspectNetwork(ctx context.Context, name string) (NetworkInfo, error)
	// ResetNetwork tears the network down (net-destroy + net-undefine) so a
	// later EnsureNetwork recreates it cleanly. Idempotent: a missing or
	// already-stopped network is treated as success.
	ResetNetwork(ctx context.Context, name string) error
}

// NetworkPreflighter is an optional capability a NetworkProvisioner may
// implement to declare a host-level readiness check beyond hypervisor
// reachability. The macOS vmnet-helper backend uses it to verify the helper
// binary is installed and runnable under passwordless sudo; libvirt does not
// implement it. Stages call it via a type assertion, so adding it does not
// change the core NetworkProvisioner contract.
type NetworkPreflighter interface {
	NetworkPreflight(ctx context.Context) error
}

// InstallerSpec carries everything the Installer needs for one call. Binary
// paths live here so the same Installer can service clusters built against
// different OCP versions.
type InstallerSpec struct {
	ClusterDir          string
	Cluster             *config.ClusterConfig
	PullSecret          string
	SSHPublicKey        string
	InstallerPath       string
	CoreOSInstallerPath string
	// Arch is the payload architecture key ("x86_64" / "aarch64") for mirror +
	// stream-json lookups. Set from runtime.GOARCH by the InstallerSpec builder;
	// empty defaults to "x86_64".
	Arch string
	// Out, if non-nil, receives the streaming stdout+stderr of the wait-for
	// commands (stages tee it to terminal + log file).
	Out io.Writer
}

// Installer abstracts invocations of openshift-install and coreos-installer.
type Installer interface {
	WriteInstallConfig(ctx context.Context, spec InstallerSpec) error
	CreateIgnitionConfigs(ctx context.Context, spec InstallerSpec) error
	CreateSingleNodeIgnition(ctx context.Context, spec InstallerSpec) error
	EmbedIgnitionInISO(ctx context.Context, spec InstallerSpec, isoPath, ignitionPath, outputPath string) error
	// EmbedNetworkKeyfileInISO embeds a NetworkManager keyfile into the live
	// ISO (coreos-installer iso network embed) so the node applies static
	// networking from first boot. Used in bridge mode to pin the master IP
	// and eliminate the DHCP race. Operates in place on isoPath.
	EmbedNetworkKeyfileInISO(ctx context.Context, spec InstallerSpec, keyfilePath, isoPath string) error
	WaitForInstallComplete(ctx context.Context, spec InstallerSpec) error
	// CoreOSLiveISOURL returns the RHCOS live ISO URL this build pins, via
	// `openshift-install coreos print-stream-json`.
	CoreOSLiveISOURL(ctx context.Context, spec InstallerSpec) (string, error)
}

// FileServer abstracts the HTTP server that hosts ignition + RHCOS files.
type FileServer interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	RootDir() string
	BaseURL() string
}

// CSRApprover is a long-running task that approves pending CSRs against a
// cluster's kubeconfig. Run blocks until ctx is cancelled.
type CSRApprover interface {
	Run(ctx context.Context, ocPath, kubeconfigPath string) error
}

// HostnameInjector polls SSH on the master and runs `hostnamectl set-hostname`
// until ctx is cancelled, beating RHCOS's node-valid-hostname 5-min timeout
// across the live-ISO → installed-RHCOS reboot.
type HostnameInjector interface {
	Run(ctx context.Context, ip, sshKeyPath, hostname string) error
}

// DNSResolver looks up A records for a name (real impl shells out to `dig`).
type DNSResolver interface {
	Resolve(ctx context.Context, name string) ([]string, error)
}

// CertIssuer obtains a TLS certificate covering domains via ACME DNS-01.
// Returned PEM includes the leaf + intermediates.
type CertIssuer interface {
	Issue(ctx context.Context, domains []string) (certPEM, keyPEM []byte, err error)
}

// CertIssuerOpts configures construction of a CertIssuer.
type CertIssuerOpts struct {
	Email       string
	AccountDir  string
	DNSProvider string
	Token       string
	Staging     bool
}

// TrustStoreInstaller installs/removes the easyshift local CA in the host's
// trust stores (system store via sudo, NSS databases via certutil when
// present). Consumed only by cmd (the `trust` command) — never by stages.
type TrustStoreInstaller interface {
	Install(ctx context.Context, caCertPath string) error
	Uninstall(ctx context.Context, caCertPath string) error
}

// DNSManager mutates a cluster's A records (api, api-int, *.apps) on a public
// DNS provider. zone is the parent zone; fqdn is <name>.<base-domain>.
type DNSManager interface {
	Upsert(ctx context.Context, zone, fqdn, ip string) error
	Delete(ctx context.Context, zone, fqdn string) error
}

// BridgeInfo is the host's view of a Linux bridge.
type BridgeInfo struct {
	Exists bool
	Slaves []string
	Up     bool
}

// HostInspector queries the host environment for preflight checks.
type HostInspector interface {
	HasCPUVirtualization() (bool, error)
	InspectBridge(name string) (BridgeInfo, error)
	LookPath(name string) error
	AvailableDiskBytes(path string) (uint64, error)
	ARPLookup(mac string) (string, error)
	DialTCP(addr string, timeout time.Duration) error
}

// DeviceAuthPrompt is what the user must be shown to complete an OAuth
// device-code login: open the URI on any device and enter the code.
type DeviceAuthPrompt struct {
	VerificationURI string
	UserCode        string
}

// PullSecretFetcher obtains the user's OpenShift pull secret from their
// Red Hat account via the OAuth 2.0 device authorization grant. Call
// StartDeviceAuth, show the returned prompt, then WaitAndFetch, which blocks
// (polling) until the user authorizes, the code expires, or ctx is cancelled.
// The caller owns all printing.
type PullSecretFetcher interface {
	StartDeviceAuth(ctx context.Context) (DeviceAuthPrompt, error)
	WaitAndFetch(ctx context.Context) ([]byte, error)
}

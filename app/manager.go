// Package app is the assembler: it builds the in-memory environment from
// config + wired dependencies, owns the cluster lifecycle (Manager) and the
// stage Runner, and is the only package that imports concrete providers and
// stage packages.
package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/openshift"
	"github.com/TheEasyShift/easyshift/stages/allocatenetwork"
	"github.com/TheEasyShift/easyshift/stages/applytlscerts"
	"github.com/TheEasyShift/easyshift/stages/bakeimagestore"
	"github.com/TheEasyShift/easyshift/stages/createmastervms"
	"github.com/TheEasyShift/easyshift/stages/createnetwork"
	"github.com/TheEasyShift/easyshift/stages/downloadbinaries"
	"github.com/TheEasyShift/easyshift/stages/downloadrhcos"
	"github.com/TheEasyShift/easyshift/stages/embedignitioniso"
	"github.com/TheEasyShift/easyshift/stages/ensureclusterdir"
	"github.com/TheEasyShift/easyshift/stages/finalize"
	"github.com/TheEasyShift/easyshift/stages/generateignition"
	"github.com/TheEasyShift/easyshift/stages/generatesshkey"
	"github.com/TheEasyShift/easyshift/stages/mergekubeconfig"
	"github.com/TheEasyShift/easyshift/stages/publishpxeassets"
	"github.com/TheEasyShift/easyshift/stages/registercluster"
	"github.com/TheEasyShift/easyshift/stages/upsertdns"
	"github.com/TheEasyShift/easyshift/stages/verifymasterip"
	"github.com/TheEasyShift/easyshift/stages/waitforinstall"
)

const (
	defaultMasterCPUs   = 4
	defaultWorkerCPUs   = 2
	defaultMasterDiskGB = 120
	defaultWorkerDiskGB = 120
)

// ClusterManager owns the cluster lifecycle. Side-effecting work is delegated
// to Stages run by a Runner; the manager itself does validation, defaulting,
// and stage-list assembly only.
type ClusterManager struct {
	cfg    *config.Config
	deps   interfaces.Deps
	runner *Runner
}

// NewClusterManager constructs a ClusterManager backed by the given config
// and deps.
func NewClusterManager(cfg *config.Config, deps interfaces.Deps) *ClusterManager {
	return &ClusterManager{cfg: cfg, deps: deps, runner: NewRunner(cfg.ConfigDir)}
}

// buildStages assembles the create pipeline, injecting each stage with
// exactly the dependencies it needs. This is the readable top-down map of
// the lifecycle and its dependency graph.
func (cm *ClusterManager) buildStages() []interfaces.Stage {
	d := cm.deps
	// Boot media differs by host OS: Linux embeds ignition into a live ISO and
	// uploads it to a libvirt storage pool; macOS publishes PXE-style assets
	// (kernel/initrd/rootfs + ignition over HTTP) for vfkit's Linux bootloader.
	bootMedia := interfaces.Stage(embedignitioniso.New(d.Installer, d.VM))
	if runtime.GOOS == "darwin" {
		bootMedia = publishpxeassets.New(d.Files)
	}
	return []interfaces.Stage{
		registercluster.New(),
		allocatenetwork.New(),
		upsertdns.New(d.DNSManager),
		ensureclusterdir.New(),
		downloadbinaries.New(d.Download, d.Cmd, d.Host),
		downloadrhcos.New(d.Installer, d.Download),
		bakeimagestore.New(d.ImageBaker, d.Host),
		generatesshkey.New(d.Cmd, d.Host),
		generateignition.New(d.Installer, d.DNS, d.Host),
		bootMedia,
		createnetwork.New(d.Net, d.VM),
		createmastervms.New(d.VM, d.Host),
		verifymasterip.New(d.Host),
		waitforinstall.New(d.Installer, d.CSR, d.Hostname, d.VM),
		applytlscerts.New(d.NewCertIssuer, d.NewLocalCertIssuer, d.Cmd),
		mergekubeconfig.New(d.Cmd),
		finalize.New(),
	}
}

// Create provisions a cluster by running the stage pipeline, resuming from
// the first unapplied stage for an existing non-running cluster.
func (cm *ClusterManager) Create(ctx context.Context, c *config.ClusterConfig) error {
	if err := cm.validateName(c); err != nil {
		return err
	}
	if err := config.EnsurePullSecret(cm.cfg.ConfigDir); err != nil {
		return err
	}

	if existing := cm.find(c.Name); existing != nil {
		if existing.State == config.ClusterStateRunning {
			return fmt.Errorf("cluster %s is already running", c.Name)
		}
		logrus.Infof("resuming create for existing cluster %s (state=%s)", c.Name, existing.State)
		c = existing
	} else {
		cm.applyDefaults(c)
		if err := cm.validateNew(c); err != nil {
			return err
		}
	}

	// Resolve channel aliases (e.g. "stable") to a concrete version before any
	// stage runs. Idempotent: a resumed cluster already has a concrete version.
	if !openshift.IsResolvedOCPVersion(c.OCPVersion) {
		resolved, err := openshift.ResolveOCPVersion(ctx, cm.deps.Download, config.PayloadArch(runtime.GOARCH), c.OCPVersion)
		if err != nil {
			return fmt.Errorf("resolve OCP version %q: %w", c.OCPVersion, err)
		}
		logrus.Infof("resolved OCP channel %q to %s", c.OCPVersion, resolved)
		c.OCPVersion = resolved
	}

	sc := &interfaces.StageContext{Cluster: c, Config: cm.cfg}
	stages := cm.buildStages()
	if err := cm.runner.Preflight(ctx, sc, stages); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if err := cm.runner.Apply(ctx, sc, stages); err != nil {
		return err
	}

	// A successful Apply means every stage — including finalize — is recorded
	// applied, so the cluster is fully built. On a resume the runner skips
	// already-applied stages, so finalize's in-memory effect (State=running)
	// is never re-applied; without this the cluster stays stuck at "creating"
	// in config.json even though it is up. Re-assert the terminal state.
	if c.State != config.ClusterStateRunning {
		c.State = config.ClusterStateRunning
		return cm.cfg.Save()
	}
	return nil
}

// Start boots all nodes for a stopped cluster.
func (cm *ClusterManager) Start(ctx context.Context, name string) error {
	c := cm.find(name)
	if c == nil {
		return fmt.Errorf("cluster %s not found", name)
	}
	if c.State == config.ClusterStateRunning {
		return fmt.Errorf("cluster %s is already running", name)
	}
	for _, vm := range cm.vmNames(c) {
		if err := cm.deps.VM.Start(ctx, vm); err != nil {
			return fmt.Errorf("start %s: %w", vm, err)
		}
	}
	c.State = config.ClusterStateRunning
	if err := cm.cfg.Save(); err != nil {
		return err
	}
	// Convergence failure does not unmark running: the VMs are up and a
	// slow cluster may still recover; status stays accurate either way.
	return cm.convergeAfterStart(ctx, c)
}

// Stop shuts down all nodes for a running cluster.
func (cm *ClusterManager) Stop(ctx context.Context, name string) error {
	c := cm.find(name)
	if c == nil {
		return fmt.Errorf("cluster %s not found", name)
	}
	if c.State != config.ClusterStateRunning {
		return fmt.Errorf("cluster %s is not running", name)
	}
	for _, vm := range cm.vmNames(c) {
		if err := cm.deps.VM.Stop(ctx, vm); err != nil {
			logrus.Warnf("stop %s: %v", vm, err)
		}
	}
	c.State = config.ClusterStateStopped
	return cm.cfg.Save()
}

// Delete tears down a cluster by rolling back every applied stage, then
// removes the cluster dir.
func (cm *ClusterManager) Delete(ctx context.Context, name string) error {
	c := cm.find(name)
	if c == nil {
		return fmt.Errorf("cluster %s not found", name)
	}
	if c.State == config.ClusterStateRunning {
		if err := cm.Stop(ctx, name); err != nil {
			return err
		}
	}

	sc := &interfaces.StageContext{Cluster: c, Config: cm.cfg}
	if err := cm.runner.Rollback(ctx, sc, cm.buildStages()); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}

	// The runner re-creates the cluster dir when persisting state after each
	// rollback step; with all rollbacks done it has no purpose — wipe it so a
	// future Create is fresh.
	_ = os.RemoveAll(config.ClusterDir(cm.cfg.ConfigDir, name))
	return cm.cfg.Save()
}

// List returns all known clusters.
func (cm *ClusterManager) List() []*config.ClusterConfig {
	out := make([]*config.ClusterConfig, len(cm.cfg.Clusters))
	copy(out, cm.cfg.Clusters)
	return out
}

func (cm *ClusterManager) find(name string) *config.ClusterConfig {
	for _, c := range cm.cfg.Clusters {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (cm *ClusterManager) validateName(c *config.ClusterConfig) error {
	if c.Name == "" {
		return fmt.Errorf("cluster name is required")
	}
	return nil
}

// resolveMagicDNS turns the --magic-dns flag value ("auto"/"off"/service)
// into a concrete service on c.MagicDNS ("" means off). Auto picks sslip.io
// for NAT mode when the user didn't supply their own base domain; everything
// else (bridge mode, or an explicit base domain) resolves to off.
func resolveMagicDNS(c *config.ClusterConfig) {
	switch c.MagicDNS {
	case "", config.MagicDNSAuto:
		if c.NetworkMode == config.NetworkModeNAT && c.Domain == "" {
			c.MagicDNS = config.MagicDNSSslip
		} else {
			c.MagicDNS = ""
		}
	case config.MagicDNSOff:
		c.MagicDNS = ""
	}
}

// validateNew checks invariants that only apply to brand-new clusters.
func (cm *ClusterManager) validateNew(c *config.ClusterConfig) error {
	if len(cm.cfg.Clusters) >= config.DefaultClustersMax {
		return fmt.Errorf("maximum number of clusters (%d) reached", config.DefaultClustersMax)
	}
	if !config.ValidMagicDNS(c.MagicDNS) {
		return fmt.Errorf("unsupported --magic-dns %q (want auto, off, sslip.io, or nip.io)", c.MagicDNS)
	}
	if c.MagicDNS != "" {
		if c.DNSProvider != "" {
			return fmt.Errorf("--magic-dns and --dns-provider are mutually exclusive: use a wildcard service OR API-managed records, not both")
		}
		if c.Domain != "" {
			return fmt.Errorf("--magic-dns derives the base domain from the master IP; remove --base-domain")
		}
	}
	if c.MasterCount != 1 {
		return fmt.Errorf("only single-master clusters are supported")
	}
	if c.WorkerCount != 0 {
		return fmt.Errorf("phase 1 supports SNO only: WorkerCount must be 0 (add workers later via addnode)")
	}
	switch c.NetworkMode {
	case config.NetworkModeNAT:
		if c.Bridge != "" {
			return fmt.Errorf("NetworkMode=nat is incompatible with --bridge")
		}
		if c.MasterMAC != "" || c.MasterIP != "" || c.MachineCIDR != "" || c.Gateway != "" || c.DNS != "" {
			return fmt.Errorf("NetworkMode=nat assigns MAC/IP/CIDR automatically; remove --master-mac/--master-ip/--machine-cidr/--gateway/--dns")
		}
	case config.NetworkModeBridge:
		if c.Bridge == "" {
			return fmt.Errorf("NetworkMode=bridge requires --bridge (name of an existing host Linux bridge, e.g. br0)")
		}
		if c.MasterMAC == "" {
			return fmt.Errorf("NetworkMode=bridge requires --master-mac (the MAC you reserved at the router)")
		}
		if !config.ValidateMAC(c.MasterMAC) {
			return fmt.Errorf("invalid --master-mac %q", c.MasterMAC)
		}
		if c.MasterIP == "" {
			return fmt.Errorf("NetworkMode=bridge requires --master-ip (the IP the router will reserve for --master-mac)")
		}
		if !config.ValidateIP(c.MasterIP) {
			return fmt.Errorf("invalid --master-ip %q", c.MasterIP)
		}
		if c.MachineCIDR == "" {
			return fmt.Errorf("could not derive --machine-cidr; pass it explicitly")
		}
		if _, _, err := net.ParseCIDR(c.MachineCIDR); err != nil {
			return fmt.Errorf("invalid --machine-cidr %q: %w", c.MachineCIDR, err)
		}
		if c.Gateway != "" && !config.ValidateIP(c.Gateway) {
			return fmt.Errorf("invalid --gateway %q", c.Gateway)
		}
		for _, dns := range strings.Split(c.DNS, ",") {
			if dns = strings.TrimSpace(dns); dns != "" && !config.ValidateIP(dns) {
				return fmt.Errorf("invalid --dns entry %q", dns)
			}
		}
	default:
		return fmt.Errorf("invalid NetworkMode %q (want %q or %q)", c.NetworkMode, config.NetworkModeNAT, config.NetworkModeBridge)
	}
	return nil
}

func (cm *ClusterManager) applyDefaults(c *config.ClusterConfig) {
	if c.NetworkMode == "" {
		c.NetworkMode = config.NetworkModeNAT
	}
	// Resolve --magic-dns "auto"/"off" to a concrete service (or "" = off)
	// before defaulting Domain. When magic DNS is active, Domain is left
	// empty here and derived from the master IP in the allocate-network
	// stage (NAT IPs aren't known until then).
	resolveMagicDNS(c)
	if c.MagicDNS == "" && c.Domain == "" {
		c.Domain = "local"
	}
	if c.OCPVersion == "" {
		c.OCPVersion = config.DefaultOCPVersion
	}
	if c.MasterCPUs == 0 {
		c.MasterCPUs = defaultMasterCPUs
	}
	if c.WorkerCPUs == 0 {
		c.WorkerCPUs = defaultWorkerCPUs
	}
	if c.MasterDiskGB == 0 {
		c.MasterDiskGB = defaultMasterDiskGB
	}
	if c.WorkerDiskGB == 0 {
		c.WorkerDiskGB = defaultWorkerDiskGB
	}
	if c.StoragePool == "" {
		c.StoragePool = config.DefaultStoragePool
	}
	if c.NetworkMode == config.NetworkModeBridge && c.MachineCIDR == "" && c.MasterIP != "" {
		if cidr, err := config.DeriveMachineCIDR(c.MasterIP); err == nil {
			c.MachineCIDR = cidr
		}
	}
	// Default the static-network gateway to the .1 of the machine network so
	// it surfaces in config.json; the keyfile renderer derives the same value
	// as a fallback for resumed pre-existing clusters.
	if c.NetworkMode == config.NetworkModeBridge && c.Gateway == "" && c.MachineCIDR != "" {
		if gw, err := config.DeriveGateway(c.MachineCIDR); err == nil {
			c.Gateway = gw
		}
	}
}

func (cm *ClusterManager) vmNames(c *config.ClusterConfig) []string {
	names := make([]string, 0, c.MasterCount+c.WorkerCount)
	for i := 0; i < c.MasterCount; i++ {
		names = append(names, fmt.Sprintf("master-%d-%s", i, c.Name))
	}
	for i := 0; i < c.WorkerCount; i++ {
		names = append(names, fmt.Sprintf("worker-%d-%s", i, c.Name))
	}
	return names
}

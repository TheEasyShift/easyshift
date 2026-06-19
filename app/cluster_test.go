package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/app"
	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
)

// bootMediaStageName is the OS-dependent boot-media stage in the pipeline:
// macOS publishes PXE assets, Linux embeds the ignition ISO.
func bootMediaStageName() string {
	if runtime.GOOS == "darwin" {
		return "publish-pxe-assets"
	}
	return "embed-ignition-iso"
}

// newTestCluster returns a ClusterConfig suitable for the happy-path tests
// (pure SNO: 1 master, 0 workers, NAT mode).
func newTestCluster(name string) *config.ClusterConfig {
	return &config.ClusterConfig{
		Name:        name,
		Domain:      "local",
		OCPVersion:  "4.14.0",
		MasterCount: 1,
		WorkerCount: 0,
		MasterRAM:   16000,
		NetworkMode: config.NetworkModeNAT,
	}
}

// newBridgeModeCluster returns a bridge-mode cluster spec with canonical MAC
// and IP defaults so callers don't have to repeat them. Tests that exercise
// validation override the relevant fields.
func newBridgeModeCluster(name, bridge string) *config.ClusterConfig {
	c := newTestCluster(name)
	c.NetworkMode = config.NetworkModeBridge
	c.Bridge = bridge
	c.MasterMAC = testBridgeMAC
	c.MasterIP = testBridgeIP
	return c
}

// Canonical bridge-mode MAC/IP used by newBridgeModeCluster. newTestEnv seeds
// the fake host's ARP table with this pair so the verify-master-ip stage sees
// the master "come up" on its reserved IP in successful-create tests.
const (
	testBridgeMAC = "52:54:00:11:22:33"
	testBridgeIP  = "192.168.1.50"
)

// withDNSRecords seeds the fake DNS resolver with the three records bridge
// mode requires for the cluster's name + domain, all pointing at MasterIP.
// Tests that exercise the failure path skip this helper.
func withDNSRecords(bundle *fakes.Bundle, c *config.ClusterConfig) {
	fqdn := c.Name + "." + c.Domain
	bundle.DNS.Records = map[string][]string{
		"api." + fqdn:                            {c.MasterIP},
		"api-int." + fqdn:                        {c.MasterIP},
		"console-openshift-console.apps." + fqdn: {c.MasterIP},
	}
}

func newTestEnv(t *testing.T) (*config.Config, interfaces.Deps, *fakes.Bundle) {
	t.Helper()
	// Guard against the merge-kubeconfig stage writing to the developer's real
	// ~/.kube/config: redirect KUBECONFIG to a throwaway path for every test
	// that invokes Create or Delete.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig"))
	cfg := config.NewDefaultConfig(filepath.Join(t.TempDir(), "easyshift"))
	if err := config.MkdirAllForTest(cfg.ConfigDir); err != nil {
		t.Fatalf("setup config dir: %v", err)
	}
	if err := config.WritePullSecret(cfg.ConfigDir, []byte(`{"auths":{"fake":{"auth":"fake"}}}`)); err != nil {
		t.Fatalf("write fake pull secret: %v", err)
	}
	deps, bundle := fakes.All()
	// Seed the fake ARP so the verify-master-ip stage sees the bridge master
	// at its reserved IP and returns immediately (NAT tests skip the stage).
	bundle.Host.ARPTable = map[string]string{testBridgeMAC: testBridgeIP}
	return cfg, deps, bundle
}

// TestCreateCluster_BakeImages exercises the --bake-images path end-to-end
// with fakes: the bake stage builds the store, the ignition gets the
// MachineConfig + live-ISO merge, and the master gets a read-only store disk.
func TestCreateCluster_BakeImages(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("baked")
	c.BakeImages = true
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := len(bundle.ImageBaker.Baked); got != 1 {
		t.Fatalf("ImageBaker.Bake calls: got %d want 1", got)
	}
	if !bundle.Installer.WroteImageStoreManifest {
		t.Error("expected WriteImageStoreManifest to be called")
	}
	// Merging the image store into the *live ISO* ignition is part of the
	// Linux boot-media path; macOS serves ignition over HTTP (publish-pxe-assets),
	// where the equivalent merge is wired in the on-hardware phase.
	if runtime.GOOS != "darwin" && !bundle.Installer.MergedImageStoreIgnition {
		t.Error("expected MergeImageStoreIntoLiveISOIgnition to be called")
	}
	if got := len(bundle.VM.ImportedDisks); got != 1 {
		t.Fatalf("ImportDisk calls: got %d want 1", got)
	}
	// The master VM must carry exactly one read-only, shareable extra disk.
	if len(bundle.VM.Created) != 1 {
		t.Fatalf("VMs created: got %d want 1", len(bundle.VM.Created))
	}
	disks := bundle.VM.Created[0].ExtraDisks
	if len(disks) != 1 {
		t.Fatalf("master extra disks: got %d want 1", len(disks))
	}
	if !disks[0].ReadOnly || !disks[0].Shareable {
		t.Errorf("store disk must be read-only + shareable, got %+v", disks[0])
	}
}

// TestCreateCluster_NoBakeByDefault confirms baking is opt-in: a default
// cluster touches none of the bake machinery.
func TestCreateCluster_NoBakeByDefault(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	if err := mgr.Create(context.Background(), newTestCluster("plain")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(bundle.ImageBaker.Baked); got != 0 {
		t.Errorf("expected no bakes, got %d", got)
	}
	if bundle.Installer.WroteImageStoreManifest || bundle.Installer.MergedImageStoreIgnition {
		t.Error("bake-store installer methods should not be called without --bake-images")
	}
	if len(bundle.VM.Created) == 1 && len(bundle.VM.Created[0].ExtraDisks) != 0 {
		t.Errorf("master should have no extra disks without --bake-images")
	}
}

// TestCreateCluster_HappyPath walks the full stage list with all-fake deps
// and asserts each interface saw the expected call.
func TestCreateCluster_HappyPath(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("demo")
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got, want := c.State, config.ClusterStateRunning; got != want {
		t.Errorf("cluster state: got %q want %q", got, want)
	}
	if got, want := len(bundle.Net.Ensured), 1; got != want {
		t.Fatalf("EnsureNetwork calls: got %d want %d", got, want)
	}
	if got, want := bundle.Net.Ensured[0].Name, config.SharedNATNetwork; got != want {
		t.Errorf("shared network name: got %q want %q", got, want)
	}
	if got, want := len(bundle.Net.Added), 1; got != want {
		t.Fatalf("DHCP reservations: got %d want %d", got, want)
	}
	if h := bundle.Net.Added[0]; h.Network != config.SharedNATNetwork || h.Host.Hostname != "master-0-demo" {
		t.Errorf("reservation: got %+v want net=%s host=master-0-demo", h, config.SharedNATNetwork)
	}
	wantVMs := []string{"master-0-demo"}
	if got := len(bundle.VM.Created); got != len(wantVMs) {
		t.Fatalf("VMs created: got %d want %d", got, len(wantVMs))
	}
	for i, want := range wantVMs {
		if got := bundle.VM.Created[i].Name; got != want {
			t.Errorf("VM[%d] name: got %q want %q", i, got, want)
		}
	}
	// SNO + NAT: the master attaches to the shared NAT network (libvirt). On
	// macOS networking is the vmnet-helper sidecar socket, not a --network arg.
	if runtime.GOOS != "darwin" {
		if got := bundle.VM.Created[0].NetworkArg; got != "network="+config.SharedNATNetwork+",mac="+bundle.VM.Created[0].MAC+",model=virtio" {
			t.Errorf("NAT network arg: got %q", got)
		}
	}
	// Master boots from the default-pool ISO uploaded by embed-ignition-iso.
	// macOS uses the PXE boot path instead (publish-pxe-assets), so the ISO
	// import / BootISO assertions apply only off-darwin.
	if runtime.GOOS != "darwin" {
		wantISO := "/var/lib/libvirt/images/easyshift-demo-master.iso"
		if got := bundle.VM.Created[0].BootISO; got != wantISO {
			t.Errorf("master BootISO: got %q want %q", got, wantISO)
		}
		if len(bundle.VM.ImportedISOs) != 1 || bundle.VM.ImportedISOs[0] != "easyshift-demo-master.iso" {
			t.Errorf("expected ISO import of easyshift-demo-master.iso, got %v", bundle.VM.ImportedISOs)
		}
	}
	if got, want := len(cfg.Clusters), 1; got != want {
		t.Fatalf("cfg.Clusters len: got %d want %d", got, want)
	}
	// Installer interactions covering all of P1-{1,2,5,7}.
	if !bundle.Installer.WroteInstallConfig {
		t.Error("expected Installer.WriteInstallConfig to be called")
	}
	if !bundle.Installer.CreatedSingleNodeIgn {
		t.Error("expected Installer.CreateSingleNodeIgnition to be called")
	}
	if runtime.GOOS != "darwin" && !bundle.Installer.EmbeddedISO {
		t.Error("expected Installer.EmbedIgnitionInISO to be called")
	}
	if !bundle.Installer.WaitedForInstall {
		t.Error("expected Installer.WaitForInstallComplete to be called")
	}
	// Linux: 3 binaries (openshift-install, oc, coreos-installer) + 1 RHCOS ISO.
	// macOS: 2 binaries (no coreos-installer) + 3 PXE assets (kernel, initramfs,
	// rootfs) = 5 downloads on first cluster.
	wantDownloads := 4
	if runtime.GOOS == "darwin" {
		wantDownloads = 5
	}
	if got := len(bundle.Download.Calls); got != wantDownloads {
		t.Errorf("download calls: got %d want %d", got, wantDownloads)
	}
	// CSR approver goroutine was launched during wait-for-install.
	if !bundle.CSR.WasStarted() {
		t.Error("expected CSR approver to be started during wait-for-install")
	}

	// state.json should record every stage in the pipeline.
	state := readState(t, cfg.ConfigDir, "demo")
	wantStages := []string{
		"register-cluster",
		"allocate-network",
		"ensure-cluster-dir",
		"download-binaries",
		"download-rhcos",
		"bake-image-store",
		"generate-ssh-key",
		"generate-ignition",
		bootMediaStageName(),
		"create-network",
		"create-master-vms",
		"verify-master-ip",
		"upsert-dns",
		"wait-for-install",
		"apply-tls-certs",
		"merge-kubeconfig",
		"finalize",
	}
	if got, want := len(state.Stages), len(wantStages); got != want {
		t.Fatalf("state.json stages: got %d want %d (%v)", got, want, state.Stages)
	}
	for _, name := range wantStages {
		if _, ok := state.Stages[name]; !ok {
			t.Errorf("state.json missing stage %q", name)
		}
	}
}

// TestCreateCluster_ResumesAfterFailure injects a failure in the middle of
// the stage pipeline, then re-runs Create with a healed fake and asserts the
// earlier stages are NOT re-invoked.
func TestCreateCluster_ResumesAfterFailure(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)

	// Fail at create-network on the first attempt.
	wantErr := errors.New("simulated net failure")
	bundle.Net.Err = wantErr

	mgr := app.NewClusterManager(cfg, deps)
	c := newTestCluster("demo")
	err := mgr.Create(context.Background(), c)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("first Create: got err=%v want wrap of %v", err, wantErr)
	}

	// register-cluster + allocate-network + ensure-cluster-dir should have
	// applied; create-network should NOT be in state.json.
	state := readState(t, cfg.ConfigDir, "demo")
	for _, name := range []string{"register-cluster", "allocate-network", "ensure-cluster-dir"} {
		if _, ok := state.Stages[name]; !ok {
			t.Errorf("first pass: expected stage %q applied", name)
		}
	}
	if _, ok := state.Stages["create-network"]; ok {
		t.Errorf("first pass: create-network must NOT be marked applied")
	}

	// Heal the fake and re-run. Resume must pick up the existing cluster
	// from cfg.Clusters and skip already-applied stages.
	bundle.Net.Err = nil
	netCallsBefore := len(bundle.Net.Ensured)

	if err := mgr.Create(context.Background(), newTestCluster("demo")); err != nil {
		t.Fatalf("second Create: %v", err)
	}

	if got := len(bundle.Net.Ensured) - netCallsBefore; got != 1 {
		t.Errorf("expected exactly 1 new EnsureNetwork on resume, got %d", got)
	}
	if got, want := c.State, config.ClusterStateRunning; got != "" && got != want {
		// `c` here is the local pre-fail object; the real cluster object lives in cfg.Clusters.
		_ = got
	}
	final := cfg.Clusters[0]
	if got, want := final.State, config.ClusterStateRunning; got != want {
		t.Errorf("after resume, cluster state: got %q want %q", got, want)
	}
}

// TestCreateCluster_Idempotent confirms a fully-completed cluster cannot be
// re-created (error) and that re-running create after success does not
// re-invoke any side effects (because the validate path rejects it first).
func TestCreateCluster_Idempotent(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	if err := mgr.Create(context.Background(), newTestCluster("demo")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	vmCalls := len(bundle.VM.Created)
	netCalls := len(bundle.Net.Ensured)

	err := mgr.Create(context.Background(), newTestCluster("demo"))
	if err == nil {
		t.Fatal("expected error on re-create of running cluster, got nil")
	}
	if got := len(bundle.VM.Created); got != vmCalls {
		t.Errorf("VM.Created should not change on re-create, got delta %d", got-vmCalls)
	}
	if got := len(bundle.Net.Ensured); got != netCalls {
		t.Errorf("Net.Ensured should not change on re-create, got delta %d", got-netCalls)
	}
}

// TestCreateCluster_HealsStuckCreatingState reproduces a config/state divergence:
// state.json records finalize as applied, but config.json still shows the cluster
// as "creating" (a stale Save clobbered the running state). Re-running create must
// self-heal it to running even though finalize is skipped as already-applied.
func TestCreateCluster_HealsStuckCreatingState(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	if err := mgr.Create(context.Background(), newTestCluster("demo")); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Simulate the clobber: persisted state regresses to creating while
	// state.json keeps every stage (including finalize) applied.
	cfg.Clusters[0].State = config.ClusterStateCreating
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	vmCalls := len(bundle.VM.Created)

	if err := mgr.Create(context.Background(), newTestCluster("demo")); err != nil {
		t.Fatalf("re-create to heal: %v", err)
	}
	if got, want := cfg.Clusters[0].State, config.ClusterStateRunning; got != want {
		t.Errorf("state not healed: got %q want %q", got, want)
	}
	// All stages already applied, so nothing should re-run on the healing pass.
	if got := len(bundle.VM.Created); got != vmCalls {
		t.Errorf("VM.Created changed on heal (delta %d); stages should be skipped", got-vmCalls)
	}
}

// TestDeleteCluster_RollsBackAppliedStages confirms Delete walks the stage
// list in reverse and invokes VM.Delete + Net.RemoveHost (the shared network
// itself is left intact).
func TestDeleteCluster_RollsBackAppliedStages(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	if err := mgr.Create(context.Background(), newTestCluster("demo")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Delete(context.Background(), "demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// SNO: only the master VM should be deleted (Stop runs first via Delete's
	// own pre-step because state == running).
	if got, want := len(bundle.VM.Deleted), 1; got != want {
		t.Errorf("VM.Deleted: got %d want %d", got, want)
	}
	if got, want := len(bundle.Net.Removed), 1; got != want {
		t.Errorf("Net.Removed (DHCP reservation) : got %d want %d", got, want)
	}
	if got := len(bundle.Net.Ensured); got == 0 {
		t.Error("expected the shared network to have been ensured during create")
	}
	if got, want := len(cfg.Clusters), 0; got != want {
		t.Errorf("cfg.Clusters len after delete: got %d want %d", got, want)
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "clusters", "demo", "state.json")); !os.IsNotExist(err) {
		t.Errorf("state.json should be removed after delete; stat err: %v", err)
	}
	// The cluster dir itself must also be gone — otherwise a fresh Create with
	// the same name would inherit stale auth/kubeconfig + ignition files.
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "clusters", "demo")); !os.IsNotExist(err) {
		t.Errorf("cluster dir should be removed after delete; stat err: %v", err)
	}
}

// TestCreateCluster_ResolvesStableChannel confirms that a cluster created
// with OCPVersion="stable" has its version replaced with the concrete
// version named in the mirror's release.txt before any stage runs.
func TestCreateCluster_ResolvesStableChannel(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("auto")
	c.OCPVersion = config.OCPChannelStable

	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.OCPVersion != "4.99.0" {
		t.Errorf("expected resolved version 4.99.0, got %q", c.OCPVersion)
	}

	var sawReleaseTxt bool
	for _, call := range bundle.Download.Calls {
		if strings.HasSuffix(call.URL, "/release.txt") {
			sawReleaseTxt = true
			if !strings.Contains(call.URL, "/stable/") {
				t.Errorf("expected stable channel URL, got %q", call.URL)
			}
			break
		}
	}
	if !sawReleaseTxt {
		t.Error("expected a release.txt download to resolve the version")
	}

	// Subsequent download stages should use the resolved version, not "stable".
	binDir := config.BinariesDir(cfg.ConfigDir, "4.99.0")
	var sawResolvedBinDir bool
	for _, call := range bundle.Download.Calls {
		if strings.Contains(call.DestPath, binDir) {
			sawResolvedBinDir = true
			break
		}
	}
	if !sawResolvedBinDir {
		t.Errorf("expected at least one download under %s, got calls: %+v", binDir, bundle.Download.Calls)
	}

	// The Installer and CSR approver must receive the resolved-version
	// binary paths, NOT the unresolved alias. This is the regression that
	// caused "openshift-install not found in bin/stable/".
	wantInstaller := filepath.Join(binDir, "openshift-install")
	if got := bundle.Installer.LastSpec.InstallerPath; got != wantInstaller {
		t.Errorf("Installer.InstallerPath: got %q want %q", got, wantInstaller)
	}
	wantCoreos := filepath.Join(binDir, "coreos-installer")
	if got := bundle.Installer.LastSpec.CoreOSInstallerPath; got != wantCoreos {
		t.Errorf("Installer.CoreOSInstallerPath: got %q want %q", got, wantCoreos)
	}
	wantOC := filepath.Join(binDir, "oc")
	if got := bundle.CSR.LastOCPath; got != wantOC {
		t.Errorf("CSR.LastOCPath: got %q want %q", got, wantOC)
	}
}

// TestCreateCluster_PreflightFailsWhenMissingBinaries confirms LookPath
// preflights surface clearly when host binaries are missing.
func TestCreateCluster_PreflightFailsWhenMissingBinaries(t *testing.T) {
	bootBinary := "virt-install"
	if runtime.GOOS == "darwin" {
		bootBinary = "vfkit"
	}
	for _, missing := range []string{"tar", "ssh-keygen", bootBinary} {
		t.Run(missing, func(t *testing.T) {
			cfg, deps, bundle := newTestEnv(t)
			bundle.Host.MissingBinaries = map[string]bool{missing: true}

			mgr := app.NewClusterManager(cfg, deps)
			err := mgr.Create(context.Background(), newTestCluster("no"+missing))
			if err == nil {
				t.Fatalf("expected preflight error for missing %q", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error should name the missing binary %q: %v", missing, err)
			}
			if got := len(bundle.VM.Created); got != 0 {
				t.Errorf("no VMs should be created on preflight failure, got %d", got)
			}
		})
	}
}

// TestCreateCluster_PreflightFailsWithoutCPUVirt confirms the host CPU
// virtualization check stops Create before VM creation is attempted.
func TestCreateCluster_PreflightFailsWithoutCPUVirt(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.Host.NoVirtualization = true

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("novt"))
	if err == nil {
		t.Fatal("expected preflight error for missing CPU virtualization")
	}
	if !strings.Contains(err.Error(), "vmx") && !strings.Contains(err.Error(), "svm") {
		t.Errorf("error should mention vmx/svm: %v", err)
	}
}

// TestCreateCluster_PreflightFailsOnLowDisk confirms low-disk preflight.
func TestCreateCluster_PreflightFailsOnLowDisk(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	// Cluster wants 120 GiB master disk; advertise 1 GiB free.
	bundle.Host.DiskAvailable = 1 << 30

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("nodisk"))
	if err == nil {
		t.Fatal("expected preflight error for insufficient disk")
	}
	if !strings.Contains(err.Error(), "disk") {
		t.Errorf("error should mention disk: %v", err)
	}
}

// TestCreateCluster_PreflightFailsOnMissingBridge confirms bridge-mode
// preflight checks the named host bridge actually exists.
func TestCreateCluster_PreflightFailsOnMissingBridge(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.Host.MissingBridges = map[string]bool{"br99": true}

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newBridgeModeCluster("nobridge", "br99"))
	if err == nil {
		t.Fatal("expected preflight error for missing bridge")
	}
	if !strings.Contains(err.Error(), "br99") {
		t.Errorf("error should name the missing bridge: %v", err)
	}
}

// TestCreateCluster_PreflightFailsOnBridgeWithNoSlaves catches the trap that
// burned us in real use: br0 exists but is empty, so the VM boots with no
// path to the LAN and openshift-install times out hours later.
func TestCreateCluster_PreflightFailsOnBridgeWithNoSlaves(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.Host.Bridges = map[string]interfaces.BridgeInfo{
		"br0": {Exists: true, Slaves: nil, Up: false},
	}

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newBridgeModeCluster("noslaves", "br0"))
	if err == nil {
		t.Fatal("expected preflight error for empty bridge")
	}
	if !strings.Contains(err.Error(), "no slave interfaces") {
		t.Errorf("error should explain the empty-bridge problem: %v", err)
	}
}

// TestCreateCluster_PreflightFailsOnBridgeDown confirms an enslaved-but-down
// bridge is rejected with a hint to bring it up.
func TestCreateCluster_PreflightFailsOnBridgeDown(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.Host.Bridges = map[string]interfaces.BridgeInfo{
		"br0": {Exists: true, Slaves: []string{"enp1s0"}, Up: false},
	}

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newBridgeModeCluster("brdown", "br0"))
	if err == nil {
		t.Fatal("expected preflight error for down bridge")
	}
	if !strings.Contains(err.Error(), "not up") {
		t.Errorf("error should say the bridge is not up: %v", err)
	}
}

// TestCreateCluster_PreflightFailsOnInvalidPullSecret confirms the JSON
// validity preflight catches malformed pull secrets.
func TestCreateCluster_PreflightFailsOnInvalidPullSecret(t *testing.T) {
	cfg := config.NewDefaultConfig(filepath.Join(t.TempDir(), "easyshift"))
	if err := config.MkdirAllForTest(cfg.ConfigDir); err != nil {
		t.Fatalf("setup config dir: %v", err)
	}
	if err := config.WritePullSecret(cfg.ConfigDir, []byte("not valid json {")); err != nil {
		t.Fatalf("write bad pull secret: %v", err)
	}
	deps, _ := fakes.All()
	mgr := app.NewClusterManager(cfg, deps)

	err := mgr.Create(context.Background(), newTestCluster("bad"))
	if err == nil {
		t.Fatal("expected preflight error for invalid pull secret JSON")
	}
	if !strings.Contains(err.Error(), "JSON") && !strings.Contains(err.Error(), "json") {
		t.Errorf("error should mention JSON: %v", err)
	}
}

// TestCreateCluster_PreflightAggregatesFailures confirms multiple failing
// preflights are reported together, not one-at-a-time.
func TestCreateCluster_PreflightAggregatesFailures(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	// The boot binary differs by OS: virt-install on Linux, vfkit on macOS.
	bootBinary := "virt-install"
	if runtime.GOOS == "darwin" {
		bootBinary = "vfkit"
	}
	bundle.Host.MissingBinaries = map[string]bool{"tar": true, "ssh-keygen": true, bootBinary: true}
	bundle.Host.NoVirtualization = true

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("multi"))
	if err == nil {
		t.Fatal("expected aggregated preflight error")
	}
	msg := err.Error()
	for _, want := range []string{"tar", "ssh-keygen", bootBinary} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q: %v", want, err)
		}
	}
}

// TestCreateCluster_HonorsCustomStoragePool confirms a non-default pool name
// (e.g. "images") flows through to the disk spec and the ISO import.
func TestCreateCluster_HonorsCustomStoragePool(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newTestCluster("custompool")
	c.StoragePool = "images"

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := bundle.VM.Created[0].StoragePool; got != "images" {
		t.Errorf("disk StoragePool: got %q want images", got)
	}
}

// newNATCluster returns a NAT-mode cluster with no base domain, so magic-DNS
// auto kicks in.
func newNATCluster(name string) *config.ClusterConfig {
	return &config.ClusterConfig{
		Name:        name,
		OCPVersion:  "4.14.0",
		MasterCount: 1,
		WorkerCount: 0,
		MasterRAM:   16000,
		NetworkMode: config.NetworkModeNAT,
		MagicDNS:    config.MagicDNSAuto,
	}
}

// TestCreateCluster_NAT_MagicDNS confirms the zero-config NAT path: magic-DNS
// auto resolves to sslip.io, the base domain is derived from the allocated
// NAT IP, the libvirt network gets a DHCP reservation (with the master
// hostname, no <domain> element), and no DNS preflight runs.
func TestCreateCluster_NAT_MagicDNS(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newNATCluster("natmagic")
	// Intentionally seed NO DNS records: if the resolver preflight ran, Create
	// would fail. It must be skipped under magic-DNS.

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if c.MagicDNS != config.MagicDNSSslip {
		t.Errorf("MagicDNS resolved: got %q want %q", c.MagicDNS, config.MagicDNSSslip)
	}
	// First NAT address is 192.168.126.5 (BaseNetworkRange + NetworkStart).
	wantDomain := "192.168.126.5.sslip.io"
	if c.Domain != wantDomain {
		t.Errorf("derived domain: got %q want %q", c.Domain, wantDomain)
	}
	// One shared NAT network ensured, with no <domain> (magic DNS forwards
	// upstream).
	if got := len(bundle.Net.Ensured); got != 1 {
		t.Fatalf("EnsureNetwork calls: got %d want 1", got)
	}
	if ens := bundle.Net.Ensured[0]; ens.Name != config.SharedNATNetwork || ens.Domain != "" {
		t.Errorf("shared net: got %+v want name=%s domain=\"\"", ens, config.SharedNATNetwork)
	}
	// One DHCP reservation pinning the derived IP with a cluster-unique hostname.
	if got := len(bundle.Net.Added); got != 1 {
		t.Fatalf("AddHost calls: got %d want 1", got)
	}
	h := bundle.Net.Added[0].Host
	if h.IP != "192.168.126.5" || h.Hostname != "master-0-natmagic" || h.MAC == "" {
		t.Errorf("reservation: got %+v want IP=192.168.126.5 host=master-0-natmagic mac!=\"\"", h)
	}
}

// TestCreateCluster_NAT_SharedNetwork confirms two NAT clusters attach to the
// single shared network (each EnsureNetwork targets the same name), each adds
// its own distinct DHCP reservation, and deleting one removes only that
// cluster's reservation — the shared network and the other cluster's
// reservation are left intact. This is the property DR topologies rely on
// (clusters on one L2 segment can talk to each other).
func TestCreateCluster_NAT_SharedNetwork(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	if err := mgr.Create(context.Background(), newNATCluster("hub")); err != nil {
		t.Fatalf("create hub: %v", err)
	}
	if err := mgr.Create(context.Background(), newNATCluster("spoke")); err != nil {
		t.Fatalf("create spoke: %v", err)
	}

	// Both clusters ensured the SAME shared network.
	for i, e := range bundle.Net.Ensured {
		if e.Name != config.SharedNATNetwork {
			t.Errorf("Ensured[%d] name: got %q want %q", i, e.Name, config.SharedNATNetwork)
		}
	}
	// Two distinct reservations on the shared network (.5 and .6).
	if got := len(bundle.Net.Added); got != 2 {
		t.Fatalf("AddHost calls: got %d want 2 (%+v)", got, bundle.Net.Added)
	}
	gotIPs := map[string]string{} // ip -> hostname
	for _, a := range bundle.Net.Added {
		if a.Network != config.SharedNATNetwork {
			t.Errorf("reservation on wrong network: %+v", a)
		}
		gotIPs[a.Host.IP] = a.Host.Hostname
	}
	if gotIPs["192.168.126.5"] != "master-0-hub" || gotIPs["192.168.126.6"] != "master-0-spoke" {
		t.Errorf("expected distinct reservations .5->hub .6->spoke, got %v", gotIPs)
	}

	// Delete the hub: only its reservation is removed; the shared network and
	// spoke's reservation are untouched (no whole-network teardown).
	if err := mgr.Delete(context.Background(), "hub"); err != nil {
		t.Fatalf("delete hub: %v", err)
	}
	if got := len(bundle.Net.Removed); got != 1 {
		t.Fatalf("RemoveHost calls: got %d want 1 (%+v)", got, bundle.Net.Removed)
	}
	if r := bundle.Net.Removed[0]; r.Host.Hostname != "master-0-hub" {
		t.Errorf("removed wrong reservation: got %+v want host=master-0-hub", r)
	}
}

// TestCreateCluster_MagicDNS_RejectsDNSProvider confirms magic-DNS and
// --dns-provider are mutually exclusive.
func TestCreateCluster_MagicDNS_RejectsDNSProvider(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	c := newNATCluster("conflict")
	c.MagicDNS = config.MagicDNSSslip
	c.DNSProvider = config.DNSProviderCloudflare

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

// TestCreateCluster_MagicDNS_RejectsExplicitBaseDomain confirms forcing a
// magic-DNS service while also setting a base domain is rejected.
func TestCreateCluster_MagicDNS_RejectsExplicitBaseDomain(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	c := newNATCluster("conflict2")
	c.MagicDNS = config.MagicDNSSslip
	c.Domain = "example.com"

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "remove --base-domain") {
		t.Fatalf("expected explicit-base-domain rejection, got %v", err)
	}
}

// TestCreateCluster_PreflightFailsWhenDefaultPoolMissing confirms the
// storage-pool preflight aborts Create when the default pool is absent.
func TestCreateCluster_PreflightFailsWhenDefaultPoolMissing(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.VM.StoragePoolErr = errors.New(`libvirt storage pool "default" not found`)

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("nopool"))
	if err == nil {
		t.Fatal("expected preflight error for missing default pool, got nil")
	}
	if !strings.Contains(err.Error(), "storage pool") {
		t.Errorf("error should mention storage pool: %v", err)
	}
	if got := len(bundle.VM.Created); got != 0 {
		t.Errorf("no VMs should be created on preflight failure, got %d", got)
	}
}

// TestCreateCluster_PreflightFailsWhenDefaultPoolInactive confirms the
// preflight catches a defined-but-stopped default pool.
func TestCreateCluster_PreflightFailsWhenDefaultPoolInactive(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.VM.StoragePoolErr = errors.New(`libvirt storage pool "default" exists but is not running`)

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("inactivepool"))
	if err == nil {
		t.Fatal("expected preflight error for inactive default pool, got nil")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("error should mention the pool is not running: %v", err)
	}
}

// TestCreateCluster_PreflightFailsWhenLibvirtUnreachable confirms the
// preflight check on createMasterVMsStage aborts Create before any stage
// runs when qemu:///system is unreachable.
func TestCreateCluster_PreflightFailsWhenLibvirtUnreachable(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	bundle.VM.CheckAccessErr = errors.New("libvirt at qemu:///system is not reachable: Permission denied")

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), newTestCluster("nolibvirt"))
	if err == nil {
		t.Fatal("expected preflight error, got nil")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("error message should mention preflight: %v", err)
	}
	// Nothing should have been applied: no VM created, no network created,
	// no cluster in cfg.
	if got := len(bundle.VM.Created); got != 0 {
		t.Errorf("preflight failure must not create VMs, got %d", got)
	}
	if got := len(bundle.Net.Ensured); got != 0 {
		t.Errorf("preflight failure must not create networks, got %d", got)
	}
	if got := len(cfg.Clusters); got != 0 {
		t.Errorf("preflight failure must not register cluster, got %d", got)
	}
}

// TestCreateCluster_DNSAutomation_UpsertsRecords confirms that setting
// DNSProvider on the cluster causes the upsert-dns stage to call the
// configured DNS manager with the expected zone/fqdn/ip — and that delete
// rolls those records back.
func TestCreateCluster_DNSAutomation_UpsertsRecords(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("dnsauto", "br0")
	c.DNSProvider = config.DNSProviderCloudflare
	// No DNS records seeded — preflight should skip the resolver check
	// because easyshift owns the records when DNSProvider is set.
	// Also write a fake token so EnsureDNSToken passes.
	if err := config.WriteDNSToken(cfg.ConfigDir, config.DNSProviderCloudflare, []byte("fake-token")); err != nil {
		t.Fatalf("write fake token: %v", err)
	}

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got, n := len(bundle.DNSManager.Upserts), 1; got != n {
		t.Fatalf("Upsert calls: got %d want %d (%+v)", got, n, bundle.DNSManager.Upserts)
	}
	call := bundle.DNSManager.Upserts[0]
	if call.Zone != c.Domain || call.FQDN != c.Name+"."+c.Domain || call.IP != c.MasterIP {
		t.Errorf("Upsert args: got %+v want zone=%s fqdn=%s ip=%s",
			call, c.Domain, c.Name+"."+c.Domain, c.MasterIP)
	}

	// Delete should drive a Delete call against the same zone/fqdn.
	if err := mgr.Delete(context.Background(), c.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, n := len(bundle.DNSManager.Deletes), 1; got != n {
		t.Fatalf("Delete calls: got %d want %d", got, n)
	}
	if d := bundle.DNSManager.Deletes[0]; d.Zone != c.Domain || d.FQDN != c.Name+"."+c.Domain {
		t.Errorf("Delete args: got %+v want zone=%s fqdn=%s", d, c.Domain, c.Name+"."+c.Domain)
	}
}

// TestCreateCluster_DNSAutomation_RequiresToken confirms the preflight
// fails when DNSProvider is set but the corresponding token file is
// missing — fail-fast before any side effects.
func TestCreateCluster_DNSAutomation_RequiresToken(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("notoken", "br0")
	c.DNSProvider = config.DNSProviderCloudflare
	// Intentionally no WriteDNSToken call.
	_ = bundle

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil {
		t.Fatal("expected token-missing preflight error, got nil")
	}
	if !strings.Contains(err.Error(), "easyshift dns set") {
		t.Errorf("error should suggest the fix command: %v", err)
	}
}

// TestCreateCluster_NoDNSAutomation_PreservesPreflight confirms the
// original behavior — when DNSProvider is empty, the bridge-mode DNS
// preflight still runs and complains about missing records.
func TestCreateCluster_NoDNSAutomation_PreservesPreflight(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("manualdns", "br0")
	c.DNSProvider = "" // manual mode
	_ = bundle         // no withDNSRecords on purpose

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil {
		t.Fatal("expected DNS preflight failure in manual mode, got nil")
	}
	if !strings.Contains(err.Error(), "DNS") {
		t.Errorf("error should mention DNS: %v", err)
	}
}

// TestCreateCluster_TLSAutomation_IssuesAndApplies confirms that setting
// TLSEmail drives the apply-tls-certs stage: two certs issued (api +
// *.apps), the issuer is constructed with the right per-cluster opts, and
// the cluster gets the patches via oc.
func TestCreateCluster_TLSAutomation_IssuesAndApplies(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("tlsauto", "br0")
	c.DNSProvider = config.DNSProviderCloudflare
	c.TLSEmail = "ops@example.com"
	c.TLSStaging = true
	if err := config.WriteDNSToken(cfg.ConfigDir, config.DNSProviderCloudflare, []byte("fake-token")); err != nil {
		t.Fatalf("write fake token: %v", err)
	}

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Two cert issuances, both for the cluster's FQDN.
	fqdn := c.Name + "." + c.Domain
	want := [][]string{{"api." + fqdn}, {"*.apps." + fqdn}}
	if got := bundle.CertIssuer.Issued; !reflect.DeepEqual(got, want) {
		t.Errorf("CertIssuer.Issued: got %v want %v", got, want)
	}

	// Issuer was constructed with the per-cluster opts.
	opts := bundle.CertIssuer.LastOpts
	if opts.Email != c.TLSEmail || !opts.Staging || opts.DNSProvider != c.DNSProvider {
		t.Errorf("LastOpts: got %+v; expected email=%s staging=true dns=%s",
			opts, c.TLSEmail, c.DNSProvider)
	}

	// Cluster got the two patch commands.
	var sawAPIPatch, sawIngressPatch bool
	for _, call := range bundle.Cmd.Calls {
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "apiserver/cluster") && strings.Contains(joined, "patch") {
			sawAPIPatch = true
		}
		if strings.Contains(joined, "ingresscontroller/default") && strings.Contains(joined, "patch") {
			sawIngressPatch = true
		}
	}
	if !sawAPIPatch {
		t.Error("expected oc patch apiserver/cluster")
	}
	if !sawIngressPatch {
		t.Error("expected oc patch ingresscontroller/default")
	}
}

// TestCreateCluster_TLSRequiresDNSProvider confirms the apply-tls-certs
// preflight rejects TLSEmail-without-DNSProvider — TLS needs DNS-01,
// which needs DNS-provider credentials.
func TestCreateCluster_TLSRequiresDNSProvider(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("tlsnodns", "br0")
	c.TLSEmail = "ops@example.com"
	// DNSProvider deliberately empty.
	withDNSRecords(bundle, c) // make the DNS resolver preflight happy

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil {
		t.Fatal("expected preflight error for TLS-without-DNS, got nil")
	}
	if !strings.Contains(err.Error(), "dns-provider") {
		t.Errorf("error should mention --dns-provider: %v", err)
	}
}

// TestCreateCluster_NoTLS_SkipsStage confirms the stage no-ops when
// TLSEmail is empty — no Issue calls, no patches.
func TestCreateCluster_NoTLS_SkipsStage(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("notls", "br0")
	withDNSRecords(bundle, c)

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(bundle.CertIssuer.Issued); got != 0 {
		t.Errorf("CertIssuer.Issued should be empty when TLS is off; got %d entries", got)
	}
}

// TestCreateCluster_RetriesInstallTimeout confirms wait-for-install re-runs
// openshift-install when it exits with status 6 ("timed out waiting for the
// condition"), up to waitForInstallRetries times. Without this, SNO
// bootstrap-in-place installs that exceed the 40-min initialization budget
// (a common case once mco-firstboot does an ostree pivot) bubble up as
// stage failures even though the cluster is still healthily progressing.
func TestCreateCluster_RetriesInstallTimeout(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("retrytest", "br0")
	withDNSRecords(bundle, c)
	bundle.Installer.WaitForInstallTimeouts = 2 // fail twice, succeed on 3rd

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create with 2 transient timeouts should succeed: %v", err)
	}
	if got, want := bundle.Installer.WaitForInstallCalls, 3; got != want {
		t.Errorf("WaitForInstallComplete call count: got %d want %d", got, want)
	}
}

// TestCreateCluster_GivesUpAfterMaxInstallTimeouts confirms the retry loop
// has a ceiling — repeated timeouts surface as a stage failure rather than
// looping forever.
func TestCreateCluster_GivesUpAfterMaxInstallTimeouts(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("toomany", "br0")
	withDNSRecords(bundle, c)
	bundle.Installer.WaitForInstallTimeouts = 99 // always fail

	mgr := app.NewClusterManager(cfg, deps)
	err := mgr.Create(context.Background(), c)
	if err == nil {
		t.Fatal("expected sustained timeouts to fail Create")
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("error should mention give-up: %v", err)
	}
}

// TestCreateCluster_LaunchesHostnameInjector confirms wait-for-install starts
// the hostname injector goroutine, which keeps SSHing into the master and
// running `hostnamectl set-hostname` until install-complete returns. We need
// this because RHCOS bootstrap-in-place ignores `coreos.inst.persistent_
// kernel_args=` (its own coreos-installer driver script doesn't propagate
// live-ISO kargs), so the only window to set hostname before kubelet
// registers the node permanently is the 5-min node-valid-hostname.service
// timeout after the installed system's first boot.
func TestCreateCluster_LaunchesHostnameInjector(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("hosttest", "br0")
	withDNSRecords(bundle, c)

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := config.MasterHostname(c)
	if want != "master-0.hosttest.local" {
		t.Fatalf("MasterHostname formula changed: got %q", want)
	}
	if !bundle.Hostname.WasStarted() {
		t.Error("expected hostname injector to be started during wait-for-install")
	}
	if got := bundle.Hostname.LastHostname; got != want {
		t.Errorf("hostname injector target: got %q want %q", got, want)
	}
}

// TestCreateCluster_LaunchesHostnameInjectorNAT confirms the hostname injector
// also runs in NAT mode. The master now boots with a static NIC (no DHCP
// option-12 hostname), so SSH injection is the only thing keeping the node from
// registering permanently as localhost.
func TestCreateCluster_LaunchesHostnameInjectorNAT(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	c := newTestCluster("nathost")

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !bundle.Hostname.WasStarted() {
		t.Error("expected hostname injector to be started during wait-for-install (NAT)")
	}
	want := config.MasterHostname(c)
	if got := bundle.Hostname.LastHostname; got != want {
		t.Errorf("hostname injector target: got %q want %q", got, want)
	}
	// The injector must target the master's allocated NAT IP, not an empty
	// (bridge-only) MasterIP.
	if got, want := bundle.Hostname.LastIP, c.PrimaryMasterIP(); got != want {
		t.Errorf("hostname injector IP: got %q want %q", got, want)
	}
}

// TestCreateCluster_RejectsMissingPullSecret confirms Create errors out
// clearly when the pull secret has not been configured.
func TestCreateCluster_RejectsMissingPullSecret(t *testing.T) {
	cfg := config.NewDefaultConfig(filepath.Join(t.TempDir(), "easyshift"))
	if err := config.MkdirAllForTest(cfg.ConfigDir); err != nil {
		t.Fatalf("setup config dir: %v", err)
	}
	// Note: no pull secret is written.
	deps, _ := fakes.All()
	mgr := app.NewClusterManager(cfg, deps)

	err := mgr.Create(context.Background(), newTestCluster("nokey"))
	if err == nil {
		t.Fatal("expected error for missing pull secret, got nil")
	}
}

// TestCreateCluster_RejectsMultiMaster confirms the SNO-only invariant.
func TestCreateCluster_RejectsMultiMaster(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	err := mgr.Create(context.Background(), &config.ClusterConfig{
		Name:        "ha",
		MasterCount: 3,
		WorkerCount: 0,
		NetworkMode: config.NetworkModeNAT,
	})
	if err == nil {
		t.Fatal("expected error for MasterCount=3, got nil")
	}
}

// TestCreateCluster_RejectsWorkersInPhase1 confirms that WorkerCount>0 is
// rejected during Phase 1.
func TestCreateCluster_RejectsWorkersInPhase1(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("withworkers")
	c.WorkerCount = 2
	if err := mgr.Create(context.Background(), c); err == nil {
		t.Fatal("expected error for WorkerCount=2 in Phase 1, got nil")
	}
}

// TestCreateCluster_BridgeMode_SkipsLibvirtNetwork confirms that bridge mode
// does not call NetworkProvisioner and that the VM is attached via
// bridge=<name> using the user-supplied MAC.
func TestCreateCluster_BridgeMode_SkipsLibvirtNetwork(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("bridge mode is a Linux-only feature; deferred on macOS (vfkit uses the vmnet-helper sidecar)")
	}
	cfg, deps, bundle := newTestEnv(t)
	c := newBridgeModeCluster("bridge1", "br0")
	withDNSRecords(bundle, c)

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got := len(bundle.Net.Ensured); got != 0 {
		t.Errorf("bridge mode must NOT create libvirt network, got %d creations", got)
	}
	if got, want := len(bundle.VM.Created), 1; got != want {
		t.Fatalf("VMs created: got %d want %d", got, want)
	}
	wantArg := "bridge=br0,mac=52:54:00:11:22:33,model=virtio"
	if got := bundle.VM.Created[0].NetworkArg; got != wantArg {
		t.Errorf("bridge mode network arg:\n  got  %q\n  want %q", got, wantArg)
	}
	if got := bundle.VM.Created[0].MAC; got != "52:54:00:11:22:33" {
		t.Errorf("VM MAC: got %q want user-supplied MAC", got)
	}

	if got, want := c.MachineCIDR, "192.168.1.0/24"; got != want {
		t.Errorf("derived MachineCIDR: got %q want %q", got, want)
	}
	if got, want := c.IPAddresses, []string{"192.168.1.50"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("IP recorded: got %v want %v", got, want)
	}
}

// TestCreateCluster_BridgeMode_RequiresBridge confirms bridge mode without
// --bridge is rejected.
func TestCreateCluster_BridgeMode_RequiresBridge(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("nobridge")
	c.NetworkMode = config.NetworkModeBridge
	c.Bridge = ""
	if err := mgr.Create(context.Background(), c); err == nil {
		t.Fatal("expected error for bridge mode without --bridge, got nil")
	}
}

// TestCreateCluster_BridgeMode_RequiresMACAndIP confirms bridge mode without
// --master-mac or --master-ip is rejected up front.
func TestCreateCluster_BridgeMode_RequiresMACAndIP(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.ClusterConfig)
		want string
	}{
		{"no-mac", func(c *config.ClusterConfig) { c.MasterMAC = "" }, "master-mac"},
		{"no-ip", func(c *config.ClusterConfig) { c.MasterIP = "" }, "master-ip"},
		{"bad-mac", func(c *config.ClusterConfig) { c.MasterMAC = "not-a-mac" }, "master-mac"},
		{"bad-ip", func(c *config.ClusterConfig) { c.MasterIP = "999.999.999.999" }, "master-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, deps, _ := newTestEnv(t)
			c := newBridgeModeCluster("dm-"+tc.name, "br0")
			tc.mut(c)
			mgr := app.NewClusterManager(cfg, deps)
			err := mgr.Create(context.Background(), c)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q: %v", tc.want, err)
			}
		})
	}
}

// TestCreateCluster_BridgeMode_RejectsMACIPInNATMode confirms NAT-mode
// clusters can't sneak through with --master-mac/--master-ip/--machine-cidr.
func TestCreateCluster_BridgeMode_RejectsMACIPInNATMode(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	c := newTestCluster("nat-with-mac")
	c.MasterMAC = "52:54:00:aa:bb:cc"

	mgr := app.NewClusterManager(cfg, deps)
	if err := mgr.Create(context.Background(), c); err == nil {
		t.Fatal("expected error when NAT mode is mixed with --master-mac, got nil")
	}
}

// TestCreateCluster_BridgeMode_FailsOnBadDNS confirms the DNS preflight
// catches missing or mismatched records before any side effect.
func TestCreateCluster_BridgeMode_FailsOnBadDNS(t *testing.T) {
	t.Run("no-records", func(t *testing.T) {
		cfg, deps, bundle := newTestEnv(t)
		c := newBridgeModeCluster("nodns", "br0")
		// Intentionally omit withDNSRecords — resolver returns nil.
		_ = bundle

		mgr := app.NewClusterManager(cfg, deps)
		err := mgr.Create(context.Background(), c)
		if err == nil {
			t.Fatal("expected DNS preflight error, got nil")
		}
		if !strings.Contains(err.Error(), "DNS") {
			t.Errorf("error should mention DNS: %v", err)
		}
	})
	t.Run("wrong-ip", func(t *testing.T) {
		cfg, deps, bundle := newTestEnv(t)
		c := newBridgeModeCluster("wrongip", "br0")
		fqdn := c.Name + "." + c.Domain
		bundle.DNS.Records = map[string][]string{
			"api." + fqdn:                            {"10.99.99.99"}, // not MasterIP
			"api-int." + fqdn:                        {c.MasterIP},
			"console-openshift-console.apps." + fqdn: {c.MasterIP},
		}

		mgr := app.NewClusterManager(cfg, deps)
		err := mgr.Create(context.Background(), c)
		if err == nil {
			t.Fatal("expected DNS-mismatch error, got nil")
		}
		if !strings.Contains(err.Error(), "want "+c.MasterIP) {
			t.Errorf("error should mention the wanted IP %q: %v", c.MasterIP, err)
		}
		// Aggregated error must enumerate the full record set to create.
		if !strings.Contains(err.Error(), "api-int."+fqdn) || !strings.Contains(err.Error(), "*.apps."+fqdn) {
			t.Errorf("error should list all required records: %v", err)
		}
	})
	t.Run("dig-missing", func(t *testing.T) {
		cfg, deps, bundle := newTestEnv(t)
		c := newBridgeModeCluster("nodig", "br0")
		withDNSRecords(bundle, c)
		bundle.Host.MissingBinaries = map[string]bool{"dig": true}

		mgr := app.NewClusterManager(cfg, deps)
		err := mgr.Create(context.Background(), c)
		if err == nil {
			t.Fatal("expected error when dig is missing, got nil")
		}
		if !strings.Contains(err.Error(), "dig") {
			t.Errorf("error should mention dig: %v", err)
		}
	})
}

func readState(t *testing.T, configDir, clusterName string) *interfaces.ClusterState {
	t.Helper()
	path := filepath.Join(configDir, "clusters", clusterName, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state.json (%s): %v", path, err)
	}
	state := &interfaces.ClusterState{}
	if err := json.Unmarshal(data, state); err != nil {
		t.Fatalf("parse state.json: %v", err)
	}
	return state
}

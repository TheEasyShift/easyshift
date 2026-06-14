package interfaces

import (
	"context"
	"path/filepath"
	"runtime"
	"time"

	"github.com/TheEasyShift/easyshift/config"
)

// StageOutcome records whether a stage's Apply succeeded.
type StageOutcome string

const (
	StageOutcomeOK     StageOutcome = "ok"
	StageOutcomeFailed StageOutcome = "failed"
)

// StageRecord is the persisted result of one stage Apply.
type StageRecord struct {
	AppliedAt time.Time    `json:"appliedAt"`
	Outcome   StageOutcome `json:"outcome"`
}

// ClusterState is the on-disk record (state.json) of which stages have been
// applied to a cluster, enabling resume and precise rollback.
type ClusterState struct {
	Stages map[string]StageRecord `json:"stages"`
}

// StageContext bundles the cluster + config a Stage acts on, plus pure
// path/spec helpers derived from them. It carries NO behavioral
// dependencies — each stage holds the interfaces it needs as its own fields,
// injected at construction by app. Stages treat Cluster and Config as
// mutable; the runner persists the resulting changes.
type StageContext struct {
	Cluster *config.ClusterConfig
	Config  *config.Config
}

// ClusterDir is the cluster's openshift-install working directory.
func (sc *StageContext) ClusterDir() string {
	return config.ClusterDir(sc.Config.ConfigDir, sc.Cluster.Name)
}

// OCBinaryPath is the `oc` binary for the cluster's resolved OCP version.
func (sc *StageContext) OCBinaryPath() string {
	return filepath.Join(config.BinariesDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion), "oc")
}

// MasterISOPath is the per-cluster embedded boot ISO staged on local disk.
func (sc *StageContext) MasterISOPath() string {
	return filepath.Join(sc.ClusterDir(), "master.iso")
}

// RHCOSLiveISOPath is the cached RHCOS live ISO for the cluster's OCP version.
func (sc *StageContext) RHCOSLiveISOPath() string {
	return filepath.Join(config.RHCOSCacheDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion), "rhcos-live.iso")
}

// rhcosCache is the per-OCP-version RHCOS asset cache (shared across clusters).
func (sc *StageContext) rhcosCache() string {
	return config.RHCOSCacheDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion)
}

// RHCOSKernelPath is the cached, uncompressed RHCOS live kernel (macOS vfkit
// linux-bootloader install phase; arm64 requires an uncompressed kernel).
func (sc *StageContext) RHCOSKernelPath() string {
	return filepath.Join(sc.rhcosCache(), "vmlinuz")
}

// RHCOSInitramfsPath is the cached RHCOS live initramfs.
func (sc *StageContext) RHCOSInitramfsPath() string {
	return filepath.Join(sc.rhcosCache(), "initramfs.img")
}

// RHCOSRootfsPath is the cached RHCOS live rootfs (served over HTTP at boot).
func (sc *StageContext) RHCOSRootfsPath() string {
	return filepath.Join(sc.rhcosCache(), "rootfs.img")
}

// MasterISOVolName is the storage-pool volume name for the master ISO.
func (sc *StageContext) MasterISOVolName() string {
	return "easyshift-" + sc.Cluster.Name + "-master.iso"
}

// KubeconfigPath is the admin kubeconfig produced by the installer.
func (sc *StageContext) KubeconfigPath() string {
	return filepath.Join(sc.ClusterDir(), "auth", "kubeconfig")
}

// InstallerSpec builds the per-call InstallerSpec for a stage. Binary paths
// resolve against the cluster's resolved OCPVersion (channel aliases are
// resolved before any stage runs).
func (sc *StageContext) InstallerSpec() InstallerSpec {
	bin := config.BinariesDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion)
	return InstallerSpec{
		ClusterDir:          sc.ClusterDir(),
		Cluster:             sc.Cluster,
		InstallerPath:       filepath.Join(bin, "openshift-install"),
		CoreOSInstallerPath: filepath.Join(bin, "coreos-installer"),
		Arch:                config.PayloadArch(runtime.GOARCH),
	}
}

// Stage is one idempotent step in the cluster lifecycle. Apply must tolerate
// retry after a partial failure; Rollback undoes a successful Apply.
type Stage interface {
	Name() string
	Apply(ctx context.Context, sc *StageContext) error
	Rollback(ctx context.Context, sc *StageContext) error
}

// Preflighter is an optional interface a Stage implements to declare checks
// that must pass before ANY stage runs. The runner aggregates all failures.
type Preflighter interface {
	Preflight(ctx context.Context, sc *StageContext) error
}

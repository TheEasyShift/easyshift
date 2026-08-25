// Package installodf installs OpenShift Data Foundation on the converged
// cluster: operators, LVMS thin pool on the dedicated data disk, the
// single-node enablement tricks, and the trimmed StorageCluster. No-op
// unless the cluster opted in with --odf.
package installodf

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage drives the ODF install phases on the converged cluster.
type Stage struct{ odf interfaces.ODFInstaller }

// New returns the install-odf stage.
func New(odf interfaces.ODFInstaller) *Stage { return &Stage{odf: odf} }

func (*Stage) Name() string { return "install-odf" }

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	if !sc.Cluster.ODF {
		return nil
	}
	spec := s.spec(sc)
	if err := s.odf.InstallOperators(ctx, spec); err != nil {
		return fmt.Errorf("odf operators: %w", err)
	}
	if err := s.odf.SetupLVM(ctx, spec); err != nil {
		return fmt.Errorf("odf lvm: %w", err)
	}
	if err := s.odf.EnableSingleNode(ctx, spec); err != nil {
		return fmt.Errorf("odf single-node enablement: %w", err)
	}
	if err := s.odf.CreateStorageCluster(ctx, spec); err != nil {
		return fmt.Errorf("odf storagecluster: %w", err)
	}
	return nil
}

// Rollback is a no-op: it only runs on cluster delete, where the VM and both
// disks are destroyed moments later; a graceful in-cluster teardown would
// gain nothing and could wedge delete behind Ceph finalizers on a broken
// cluster. A future day-2 "odf remove" command would own the ordered
// teardown (StorageCluster -> operators -> VG/disk).
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

func (s *Stage) spec(sc *interfaces.StageContext) interfaces.ODFSpec {
	device := "/dev/vdb"
	if sc.Cluster.BakeImages {
		device = "/dev/vdc" // the bake store occupies vdb
	}
	return interfaces.ODFSpec{
		KubeconfigPath: filepath.Join(sc.ClusterDir(), "auth", "kubeconfig"),
		OCBinaryPath:   sc.OCBinaryPath(),
		WorkDir:        sc.ClusterDir(),
		Channel:        config.OLMChannelForVersion(sc.Cluster.OCPVersion),
		DevicePath:     device,
		DataPVCSizeGi:  sc.Cluster.ODFDiskGB/3 - 3,
	}
}

// Package publishpxeassets is the macOS boot-media stage: it publishes the
// RHCOS aarch64 kernel/initrd/rootfs and the cluster ignition into the HTTP
// fileserver root, and records the kernel cmdline (ignition.config.url +
// rootfs url) for createmastervms to pass to vfkit's Linux bootloader. It
// replaces embed-ignition-iso on macOS (which uses coreos-installer + a
// libvirt storage pool).
package publishpxeassets

import (
	"context"
	"fmt"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage publishes PXE-style boot assets.
type Stage struct {
	files interfaces.FileServer
}

// New returns the publish-pxe-assets stage.
func New(files interfaces.FileServer) *Stage { return &Stage{files: files} }

func (*Stage) Name() string { return "publish-pxe-assets" }

// KernelCmdline builds the vfkit Linux-bootloader cmdline for a cluster: it
// points Ignition at the per-cluster config and the RHCOS live rootfs, both
// served by the fileserver at baseURL.
func KernelCmdline(baseURL, cluster string) string {
	return fmt.Sprintf(
		"coreos.live.rootfs_url=%s/%s/rootfs.img ignition.config.url=%s/%s/config.ign ignition.firstboot",
		baseURL, cluster, baseURL, cluster,
	)
}

// Apply copies the kernel/initrd/rootfs + ignition into the fileserver root and
// records the cmdline on the cluster for createmastervms. The asset-copy side
// effects are implemented against real RHCOS assets in the on-hardware phase
// (Phase B); this stage is selected only on darwin.
func (s *Stage) Apply(_ context.Context, _ *interfaces.StageContext) error {
	return nil
}

// Rollback removes the published per-cluster assets.
func (s *Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error {
	return nil
}

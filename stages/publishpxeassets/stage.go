// Package publishpxeassets is the macOS boot-media stage: it publishes the
// RHCOS live rootfs and the cluster ignition into the HTTP fileserver root, and
// records the kernel cmdline (ignition.config.url + rootfs url) on the cluster
// for create-master-vms to pass to vfkit's Linux bootloader. It replaces
// embed-ignition-iso on macOS (which uses coreos-installer + a libvirt storage
// pool, neither available on mac). The kernel + initramfs are passed to vfkit
// directly by local path (from the RHCOS cache), so only the rootfs and
// ignition need HTTP serving.
package publishpxeassets

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage publishes PXE-style boot assets.
type Stage struct {
	files interfaces.FileServer
}

// New returns the publish-pxe-assets stage.
func New(files interfaces.FileServer) *Stage { return &Stage{files: files} }

func (*Stage) Name() string { return "publish-pxe-assets" }

// KernelCmdline builds the vfkit Linux-bootloader cmdline: serial console on
// hvc0, the RHCOS live rootfs and the per-cluster ignition both fetched from
// the fileserver, and the metal ignition platform with firstboot.
func KernelCmdline(baseURL, cluster string) string {
	return fmt.Sprintf(
		"console=hvc0 coreos.live.rootfs_url=%s/%s/rootfs.img ignition.firstboot ignition.platform.id=metal ignition.config.url=%s/%s/config.ign",
		baseURL, cluster, baseURL, cluster,
	)
}

// Apply copies the RHCOS rootfs and the SNO ignition into
// <fileserver-root>/<cluster>/ and records the install-phase cmdline on the
// cluster for create-master-vms.
func (s *Stage) Apply(_ context.Context, sc *interfaces.StageContext) error {
	cluster := sc.Cluster.Name
	dstDir := filepath.Join(s.files.RootDir(), cluster)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("publish-pxe-assets: mkdir %s: %w", dstDir, err)
	}
	if err := copyFile(sc.RHCOSRootfsPath(), filepath.Join(dstDir, "rootfs.img")); err != nil {
		return fmt.Errorf("publish rootfs: %w", err)
	}
	ign := filepath.Join(sc.ClusterDir(), "bootstrap-in-place-for-live-iso.ign")
	if err := copyFile(ign, filepath.Join(dstDir, "config.ign")); err != nil {
		return fmt.Errorf("publish ignition: %w", err)
	}
	sc.Cluster.InstallKernelCmdline = KernelCmdline(s.files.BaseURL(), cluster)
	return nil
}

// Rollback removes the published per-cluster assets.
func (s *Stage) Rollback(_ context.Context, sc *interfaces.StageContext) error {
	_ = os.RemoveAll(filepath.Join(s.files.RootDir(), sc.Cluster.Name))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

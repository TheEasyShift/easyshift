// Package downloadrhcos fetches the RHCOS live boot media the installer pins,
// into the shared per-version cache. On Linux this is the live ISO (booted by
// libvirt); on macOS it is the live PXE assets (kernel/initramfs/rootfs) that
// the vfkit linux-bootloader install phase boots, since macOS has no
// coreos-installer to embed ignition into an ISO.
package downloadrhcos

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage downloads the RHCOS live boot media.
type Stage struct {
	installer interfaces.Installer
	dl        interfaces.Downloader
}

// New returns the download-rhcos stage.
func New(installer interfaces.Installer, dl interfaces.Downloader) *Stage {
	return &Stage{installer: installer, dl: dl}
}

func (*Stage) Name() string { return "download-rhcos" }

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	if runtime.GOOS == "darwin" {
		return s.applyPXE(ctx, sc)
	}
	return s.applyISO(ctx, sc)
}

func (s *Stage) applyISO(ctx context.Context, sc *interfaces.StageContext) error {
	dest := sc.RHCOSLiveISOPath()
	if _, err := os.Stat(dest); err == nil {
		return nil // already cached
	}
	url, err := s.installer.CoreOSLiveISOURL(ctx, sc.InstallerSpec())
	if err != nil {
		return fmt.Errorf("determine RHCOS live ISO url: %w", err)
	}
	return s.dl.Download(ctx, url, dest)
}

// applyPXE downloads the kernel/initramfs/rootfs. The kernel is gunzipped if
// compressed, because vfkit's arm64 linux bootloader rejects a compressed
// kernel.
func (s *Stage) applyPXE(ctx context.Context, sc *interfaces.StageContext) error {
	kernel, initramfs, rootfs := sc.RHCOSKernelPath(), sc.RHCOSInitramfsPath(), sc.RHCOSRootfsPath()
	if exists(kernel) && exists(initramfs) && exists(rootfs) {
		return nil // already cached
	}
	pxe, err := s.installer.CoreOSLivePXEURLs(ctx, sc.InstallerSpec())
	if err != nil {
		return fmt.Errorf("determine RHCOS live PXE urls: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(kernel), 0o755); err != nil {
		return err
	}
	for _, d := range []struct{ url, dst string }{
		{pxe.InitramfsURL, initramfs},
		{pxe.RootfsURL, rootfs},
	} {
		if exists(d.dst) {
			continue
		}
		if err := s.dl.Download(ctx, d.url, d.dst); err != nil {
			return fmt.Errorf("download %s: %w", filepath.Base(d.dst), err)
		}
	}
	if !exists(kernel) {
		if err := s.downloadKernel(ctx, pxe.KernelURL, kernel); err != nil {
			return fmt.Errorf("download kernel: %w", err)
		}
	}
	return nil
}

// downloadKernel fetches the kernel and decompresses it if it is gzip-compressed
// (RHCOS aarch64 publishes a gzip'd Image; vfkit needs it uncompressed).
func (s *Stage) downloadKernel(ctx context.Context, url, dst string) error {
	tmp := dst + ".download"
	if err := s.dl.Download(ctx, url, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b { // gzip magic
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("gunzip kernel: %w", err)
		}
		var out bytes.Buffer
		if _, err := io.Copy(&out, zr); err != nil { //nolint:gosec // trusted mirror artifact
			return fmt.Errorf("gunzip kernel: %w", err)
		}
		data = out.Bytes()
	}
	return os.WriteFile(dst, data, 0o644)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Rollback is a no-op: the cache is shared across clusters.
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

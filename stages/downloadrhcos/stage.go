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
	"encoding/binary"
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

// downloadKernel fetches the kernel and writes it uncompressed, which vfkit's
// arm64 linux bootloader requires. RHCOS aarch64 ships the kernel as an EFI
// zboot image (a PE32+ wrapper around a gzip-compressed arm64 Image), so we
// parse the zboot header and decompress the payload.
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
	raw, err := uncompressKernel(data)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}

// uncompressKernel returns the raw (uncompressed) kernel image. It handles
// EFI zboot ("MZ"+"zimg" header → compressed payload), a bare gzip stream, and
// an already-uncompressed image (returned as-is).
func uncompressKernel(data []byte) ([]byte, error) {
	// EFI zboot: msdos "MZ" @0, "zimg" @4, payload_offset u32 @8, payload_size
	// u32 @12, compression_type[32] @0x18.
	if len(data) > 0x38 && data[0] == 'M' && data[1] == 'Z' && string(data[4:8]) == "zimg" {
		off := binary.LittleEndian.Uint32(data[8:12])
		size := binary.LittleEndian.Uint32(data[12:16])
		comp := string(bytes.TrimRight(data[0x18:0x38], "\x00"))
		if int(off)+int(size) > len(data) {
			return nil, fmt.Errorf("zboot payload [%d:%d] exceeds file size %d", off, int(off)+int(size), len(data))
		}
		payload := data[off : off+size]
		switch comp {
		case "gzip":
			return gunzip(payload)
		default:
			return nil, fmt.Errorf("unsupported zboot kernel compression %q", comp)
		}
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b { // bare gzip
		return gunzip(data)
	}
	return data, nil // already uncompressed
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("gunzip kernel: %w", err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, zr); err != nil { //nolint:gosec // trusted mirror artifact
		return nil, fmt.Errorf("gunzip kernel: %w", err)
	}
	return out.Bytes(), nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Rollback is a no-op: the cache is shared across clusters.
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

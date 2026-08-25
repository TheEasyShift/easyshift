// Package bakeimagestore pre-pulls the OCP release payload into a read-only,
// multi-arch disk image so the install serves platform images locally instead
// of pulling them from quay.io. The store is built once per OCP version and
// shared across clusters; this stage is a no-op unless the cluster opted in
// with BakeImages.
package bakeimagestore

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage builds the per-version baked image store.
type Stage struct {
	baker interfaces.ImageBaker
	host  interfaces.HostInspector
}

// New returns the bake-image-store stage.
func New(baker interfaces.ImageBaker, host interfaces.HostInspector) *Stage {
	return &Stage{baker: baker, host: host}
}

func (*Stage) Name() string { return "bake-image-store" }

// Preflight checks the bake tooling is present (only when baking is enabled and
// the store isn't already built). skopeo copies images into a rootless CRI-O
// overlay store; virt-make-fs (guestfs-tools) packs it into a labeled qcow2.
func (s *Stage) Preflight(_ context.Context, sc *interfaces.StageContext) error {
	if !sc.Cluster.BakeImages {
		return nil
	}
	if ready, err := s.baker.Ready(s.spec(sc)); err == nil && ready {
		return nil
	}
	if err := config.ValidatePullSecretJSON(sc.Config.ConfigDir); err != nil {
		return err
	}
	if err := s.host.LookPath("skopeo"); err != nil {
		return fmt.Errorf("--bake-images needs %q on PATH: %w\n  hint: %s", "skopeo", err, bakeToolHint())
	}
	if runtime.GOOS == "darwin" {
		// The packer is mke2fs (no libguestfs on macOS); Homebrew's e2fsprogs
		// is keg-only, so also probe its known keg locations.
		if !mke2fsPresent(s.host) {
			return fmt.Errorf("--bake-images needs mke2fs (none of %v found)\n  hint: %s", config.MKE2FSCandidates, bakeToolHint())
		}
		return nil
	}
	if err := s.host.LookPath("virt-make-fs"); err != nil {
		return fmt.Errorf("--bake-images needs %q on PATH: %w\n  hint: %s", "virt-make-fs", err, bakeToolHint())
	}
	return nil
}

func mke2fsPresent(host interfaces.HostInspector) bool {
	for _, c := range config.MKE2FSCandidates {
		if strings.Contains(c, "/") {
			if _, err := os.Stat(c); err == nil {
				return true
			}
			continue
		}
		if err := host.LookPath(c); err == nil {
			return true
		}
	}
	return false
}

func bakeToolHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install skopeo e2fsprogs"
	}
	return "install skopeo and guestfs-tools (Fedora/RHEL) or skopeo + libguestfs-tools (Debian/Ubuntu)"
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	if !sc.Cluster.BakeImages {
		return nil
	}
	spec := s.spec(sc)
	if ready, err := s.baker.Ready(spec); err != nil {
		return fmt.Errorf("probe baked image store: %w", err)
	} else if ready {
		return nil
	}
	if err := os.MkdirAll(config.ImageStoreCacheDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion), 0o755); err != nil {
		return err
	}
	return s.baker.Bake(ctx, spec)
}

// Rollback is a no-op: the store is a per-version cache shared across clusters,
// like the binaries cache. Deleting one cluster must not evict it.
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

func (s *Stage) spec(sc *interfaces.StageContext) interfaces.BakeSpec {
	cfgDir, version := sc.Config.ConfigDir, sc.Cluster.OCPVersion
	return interfaces.BakeSpec{
		Version:        version,
		OCBinaryPath:   sc.OCBinaryPath(),
		PullSecretPath: config.PullSecretPath(cfgDir),
		OverlayDir:     config.ImageStoreOverlayDir(cfgDir, version),
		OutputDiskPath: config.ImageStoreDiskPath(cfgDir, version),
	}
}

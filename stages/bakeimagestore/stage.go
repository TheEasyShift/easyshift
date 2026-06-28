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
	for _, tool := range []string{"skopeo", "virt-make-fs"} {
		if err := s.host.LookPath(tool); err != nil {
			return fmt.Errorf("--bake-images needs %q on PATH: %w\n  hint: install skopeo and guestfs-tools (Fedora/RHEL) or skopeo + libguestfs-tools (Debian/Ubuntu)", tool, err)
		}
	}
	return nil
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
		OutputQcowPath: config.ImageStoreQcowPath(cfgDir, version),
	}
}

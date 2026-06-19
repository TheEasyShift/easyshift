// Package downloadbinaries fetches openshift-install, oc, and
// coreos-installer into the shared per-version bin cache.
package downloadbinaries

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/openshift"
)

// Stage downloads the OCP CLI tooling.
type Stage struct {
	dl   interfaces.Downloader
	cmd  interfaces.CommandRunner
	host interfaces.HostInspector
}

// New returns the download-binaries stage.
func New(dl interfaces.Downloader, cmd interfaces.CommandRunner, host interfaces.HostInspector) *Stage {
	return &Stage{dl: dl, cmd: cmd, host: host}
}

func (*Stage) Name() string { return "download-binaries" }

// Preflight verifies `tar` is on PATH (used to extract the tarballs).
func (s *Stage) Preflight(_ context.Context, _ *interfaces.StageContext) error {
	return s.host.LookPath("tar")
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	binDir := config.BinariesDir(sc.Config.ConfigDir, sc.Cluster.OCPVersion)
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(binDir, "openshift-install")); err != nil {
		if err := s.downloadTarball(ctx, openshift.OCPClientURL(sc.Cluster.OCPVersion, "openshift-install-linux.tar.gz"), binDir); err != nil {
			return fmt.Errorf("download openshift-install: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(binDir, "oc")); err != nil {
		if err := s.downloadTarball(ctx, openshift.OCPClientURL(sc.Cluster.OCPVersion, "openshift-client-linux.tar.gz"), binDir); err != nil {
			return fmt.Errorf("download oc: %w", err)
		}
	}
	coreosPath := filepath.Join(binDir, "coreos-installer")
	if _, err := os.Stat(coreosPath); err != nil {
		if err := s.dl.Download(ctx, openshift.CoreOSInstallerURL, coreosPath); err != nil {
			return fmt.Errorf("download coreos-installer: %w", err)
		}
		if _, err := s.cmd.Run(ctx, "chmod", "+x", coreosPath); err != nil {
			return fmt.Errorf("chmod coreos-installer: %w", err)
		}
	}
	return nil
}

// Rollback is a no-op: the binaries cache is shared across clusters.
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

// downloadTarball downloads a .tar.gz, extracts it into destDir, then removes
// the tarball.
func (s *Stage) downloadTarball(ctx context.Context, url, destDir string) error {
	tmp := filepath.Join(destDir, "_download.tar.gz")
	if err := s.dl.Download(ctx, url, tmp); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := s.cmd.Run(ctx, "tar", "xzf", tmp, "-C", destDir); err != nil {
		return fmt.Errorf("extract %s: %w", tmp, err)
	}
	return nil
}

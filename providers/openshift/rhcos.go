package openshift

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// OCPClientURL constructs the mirror URL for openshift-install / openshift-client.
func OCPClientURL(version, tarball string) string {
	return fmt.Sprintf("%s/clients/ocp/%s/%s", config.OCPMirrorURL, version, tarball)
}

// CoreOSInstallerURL is the mirror URL for the latest coreos-installer binary.
const CoreOSInstallerURL = "https://mirror.openshift.com/pub/openshift-v4/clients/coreos-installer/latest/coreos-installer"

// RHCOSCacheDir returns the per-version cache root for RHCOS artifacts.
// Files here are shared across clusters of the same version.
func RHCOSCacheDir(configDir, version string) string {
	return filepath.Join(configDir, "rhcos", version)
}

// BinariesDir returns the per-version cache root for openshift-install /
// oc / coreos-installer. The same binaries get reused across clusters.
func BinariesDir(configDir, version string) string {
	return filepath.Join(configDir, "bin", version)
}

// IsResolvedOCPVersion reports whether v looks like a concrete semver
// ("4.21.0") rather than a channel alias ("stable", "latest", "candidate-4.21").
// Anything starting with a digit and containing a dot is treated as resolved.
func IsResolvedOCPVersion(v string) bool {
	if len(v) == 0 || v[0] < '0' || v[0] > '9' {
		return false
	}
	return strings.Contains(v, ".")
}

// ReleaseTxtURL is the URL to fetch the channel's release.txt index.
// The file starts with a "Name:      <version>" line that identifies which
// concrete release the alias currently points at.
func ReleaseTxtURL(channel string) string {
	return fmt.Sprintf("%s/clients/ocp/%s/release.txt", config.OCPMirrorURL, channel)
}

// ResolveOCPVersion downloads <mirror>/clients/ocp/<channel>/release.txt and
// returns the concrete version named on its first "Name:" line. The download
// goes through the supplied Downloader so tests can substitute a canned body.
func ResolveOCPVersion(ctx context.Context, dl interfaces.Downloader, channel string) (string, error) {
	tmp, err := os.CreateTemp("", "release-*.txt")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := dl.Download(ctx, ReleaseTxtURL(channel), tmp.Name()); err != nil {
		return "", fmt.Errorf("download release.txt for channel %q: %w", channel, err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", fmt.Errorf("read release.txt: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		const prefix = "Name:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("release.txt for channel %q has no Name: line", channel)
}

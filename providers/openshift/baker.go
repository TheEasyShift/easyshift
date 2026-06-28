package openshift

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// SupportedReleaseArches are the OCP release architectures easyshift bakes into
// the (multi-arch) image store. A version that doesn't publish one of these is
// skipped at bake time, so a single-arch release still works. amd64 and
// aarch64 cover Apple-silicon hosts running arm64 OCP plus Rosetta-run amd64
// workloads. Names match the `ocp-release:<version>-<arch>` tag suffix.
var SupportedReleaseArches = []string{"x86_64", "aarch64"}

// releaseImageRepo is the public repository of tagged OCP release images.
const releaseImageRepo = "quay.io/openshift-release-dev/ocp-release"

// ReleaseImageURL returns the release image ref for an OCP version + arch,
// e.g. quay.io/openshift-release-dev/ocp-release:4.21.0-x86_64.
func ReleaseImageURL(version, arch string) string {
	return fmt.Sprintf("%s:%s-%s", releaseImageRepo, version, arch)
}

// OpenShiftImageBaker implements interfaces.ImageBaker by shelling out to `oc`
// (release enumeration), `skopeo` (copy into a CRI-O overlay store), and
// `virt-make-fs` (pack the store into a labeled qcow2 — rootless via
// libguestfs). It holds no per-bake state; everything comes via BakeSpec.
type OpenShiftImageBaker struct {
	cmd interfaces.CommandRunner
}

// NewOpenShiftImageBaker returns an ImageBaker backed by cmd.
func NewOpenShiftImageBaker(cmd interfaces.CommandRunner) *OpenShiftImageBaker {
	return &OpenShiftImageBaker{cmd: cmd}
}

// Ready reports whether the packed qcow2 already exists and is non-empty, so a
// resumed create skips the multi-GB rebuild.
func (b *OpenShiftImageBaker) Ready(spec interfaces.BakeSpec) (bool, error) {
	fi, err := os.Stat(spec.OutputQcowPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Size() > 0, nil
}

// Bake enumerates the release payload for every supported arch, copies each
// image into the overlay store under their ORIGINAL names (so the digests the
// release references match with no mirror config needed), then packs the store
// into a labeled qcow2. Idempotent: skopeo skips images already present and the
// qcow2 is rebuilt from the (now-complete) store each run.
func (b *OpenShiftImageBaker) Bake(ctx context.Context, spec interfaces.BakeSpec) error {
	images, bakedArches, err := b.enumerate(ctx, spec)
	if err != nil {
		return err
	}
	if len(bakedArches) == 0 {
		return fmt.Errorf("no OCP release image found for version %s in any supported arch %v", spec.Version, SupportedReleaseArches)
	}

	if err := os.MkdirAll(spec.OverlayDir, 0o755); err != nil {
		return fmt.Errorf("create overlay store dir: %w", err)
	}
	runRoot := filepath.Join(filepath.Dir(spec.OverlayDir), "run")
	if err := os.MkdirAll(runRoot, 0o755); err != nil {
		return fmt.Errorf("create overlay run dir: %w", err)
	}

	for _, img := range images {
		dst := fmt.Sprintf("containers-storage:[overlay@%s+%s]%s", spec.OverlayDir, runRoot, img)
		if _, err := b.cmd.Run(ctx, "skopeo", "copy",
			"--all", // copy every manifest-list entry so all arches land in the store
			"--authfile", spec.PullSecretPath,
			"--retry-times", "3",
			"docker://"+img, dst,
		); err != nil {
			return fmt.Errorf("skopeo copy %s: %w", img, err)
		}
	}

	// Pack the store into a fresh labeled ext4 qcow2. virt-make-fs runs rootless
	// (libguestfs supermin), so this stays within easyshift's no-root contract.
	_ = os.Remove(spec.OutputQcowPath)
	if _, err := b.cmd.Run(ctx, "virt-make-fs",
		"--type=ext4",
		"--label="+config.BakedImagesLabel,
		"--format=qcow2",
		"--size=+1G", // headroom over the store contents for fs metadata
		spec.OverlayDir, spec.OutputQcowPath,
	); err != nil {
		return fmt.Errorf("virt-make-fs pack image store: %w", err)
	}
	return nil
}

// enumerate returns the de-duplicated union of release payload pullspecs across
// every supported arch whose release image exists, plus the list of arches that
// contributed. Each arch's release image is included alongside its components.
func (b *OpenShiftImageBaker) enumerate(ctx context.Context, spec interfaces.BakeSpec) (images, bakedArches []string, err error) {
	seen := map[string]bool{}
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			images = append(images, ref)
		}
	}
	for _, arch := range SupportedReleaseArches {
		relImg := ReleaseImageURL(spec.Version, arch)
		out, infoErr := b.cmd.Run(ctx, spec.OCBinaryPath, "adm", "release", "info",
			relImg, "--registry-config", spec.PullSecretPath, "-o", "json")
		if infoErr != nil {
			// Treat as "this arch isn't published for this version" and move on;
			// a genuine auth/network failure surfaces when no arch succeeds.
			continue
		}
		specs, parseErr := parseReleasePullspecs(out)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse release info for %s: %w", arch, parseErr)
		}
		add(relImg)
		for _, s := range specs {
			add(s)
		}
		bakedArches = append(bakedArches, arch)
	}
	return images, bakedArches, nil
}

// releaseInfo is the subset of `oc adm release info -o json` we read: the
// embedded image stream whose tags point at every component pullspec by digest.
type releaseInfo struct {
	References struct {
		Spec struct {
			Tags []struct {
				From struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"from"`
			} `json:"tags"`
		} `json:"spec"`
	} `json:"references"`
}

// parseReleasePullspecs extracts component pullspecs (DockerImage refs) from the
// JSON emitted by `oc adm release info -o json`.
func parseReleasePullspecs(data []byte) ([]string, error) {
	var ri releaseInfo
	if err := json.Unmarshal(data, &ri); err != nil {
		return nil, fmt.Errorf("unmarshal release info: %w", err)
	}
	var out []string
	for _, t := range ri.References.Spec.Tags {
		if t.From.Name != "" {
			out = append(out, t.From.Name)
		}
	}
	return out, nil
}

// --- CRI-O wiring renderers (pure) --------------------------------------

// storageConfDropinPath is where the additional-image-store drop-in lands. The
// containers/storage library merges *.conf from this dir over storage.conf.
const storageConfDropinPath = "/etc/containers/storage.conf.d/10-baked-images.conf"

// RenderStorageConfDropin returns the storage.conf drop-in that registers the
// baked store as a read-only CRI-O additional image store.
func RenderStorageConfDropin() string {
	return fmt.Sprintf(`[storage]
[storage.options]
additionalimagestores = [
  "%s",
]
`, config.BakedImagesMountPath)
}

// RenderMountUnit returns the systemd .mount unit that mounts the baked store
// disk read-only before CRI-O starts. Returned name is the systemd-escaped unit
// file name matching BakedImagesMountPath.
func RenderMountUnit() (name, contents string) {
	contents = fmt.Sprintf(`[Unit]
Description=Baked OCP image store (read-only CRI-O additional image store)
Before=crio.service
After=local-fs.target

[Mount]
What=/dev/disk/by-label/%s
Where=%s
Type=ext4
Options=ro,nofail

[Install]
RequiredBy=crio.service
`, config.BakedImagesLabel, config.BakedImagesMountPath)
	return systemdEscapePath(config.BakedImagesMountPath) + ".mount", contents
}

// MachineConfigName is the role-scoped MachineConfig that applies the baked
// store wiring to the installed node (post-pivot, the long operator-rollout
// tail). 99- prefix orders it after the rendered base config.
const MachineConfigName = "99-master-baked-image-store"

// RenderMachineConfig returns the master MachineConfig YAML that ships the
// storage.conf drop-in + mount unit to the installed node. Dropped into the
// install dir's openshift/ so `create single-node-ignition-config` renders it
// into the node's ignition — present from first boot, before CRI-O pulls
// operators.
func RenderMachineConfig() string {
	unitName, unitContents := RenderMountUnit()
	storageB64 := base64.StdEncoding.EncodeToString([]byte(RenderStorageConfDropin()))
	return fmt.Sprintf(`apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: %s
spec:
  config:
    ignition:
      version: 3.2.0
    storage:
      files:
      - path: %s
        mode: 420
        overwrite: true
        contents:
          source: data:text/plain;base64,%s
    systemd:
      units:
      - name: %s
        enabled: true
        contents: |
%s
`, MachineConfigName, storageConfDropinPath, storageB64, unitName, indent(unitContents, "          "))
}

// MergeBakedStoreIntoIgnition adds the storage.conf drop-in file and the mount
// unit to a raw Ignition config (the bootstrap-in-place-for-live-iso.ign), so
// the baked store is also used during the live-ISO bootstrap phase. It edits
// the JSON structurally to preserve whatever the installer emitted.
func MergeBakedStoreIntoIgnition(ignitionJSON []byte) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(ignitionJSON, &cfg); err != nil {
		return nil, fmt.Errorf("parse live-iso ignition: %w", err)
	}

	storage, _ := cfg["storage"].(map[string]any)
	if storage == nil {
		storage = map[string]any{}
	}
	files, _ := storage["files"].([]any)
	files = append(files, map[string]any{
		"path":      storageConfDropinPath,
		"mode":      420,
		"overwrite": true,
		"contents": map[string]any{
			"source": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(RenderStorageConfDropin())),
		},
	})
	storage["files"] = files
	cfg["storage"] = storage

	systemd, _ := cfg["systemd"].(map[string]any)
	if systemd == nil {
		systemd = map[string]any{}
	}
	units, _ := systemd["units"].([]any)
	unitName, unitContents := RenderMountUnit()
	units = append(units, map[string]any{
		"name":     unitName,
		"enabled":  true,
		"contents": unitContents,
	})
	systemd["units"] = units
	cfg["systemd"] = systemd

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("re-marshal live-iso ignition: %w", err)
	}
	return out, nil
}

// indent prefixes every non-empty line of s with prefix (for embedding a
// multi-line unit file under a YAML block scalar).
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = prefix + ln
		}
	}
	return strings.Join(lines, "\n")
}

// systemdEscapePath converts an absolute mount path to the systemd unit-name
// stem (the equivalent of `systemd-escape -p`): leading/trailing slashes
// dropped, "/" → "-", and any byte outside [A-Za-z0-9_.] (notably "-")
// rendered as \xNN. A leading "." becomes \x2e.
func systemdEscapePath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0:
			b.WriteString(`\x2e`)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

package openshift

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/template"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// coreOSStream is the subset of `openshift-install coreos print-stream-json`
// we parse. The live ISO lives at
// architectures.<arch>.artifacts.metal.formats.iso.disk.location.
type coreOSStream struct {
	Architectures map[string]struct {
		Artifacts map[string]struct {
			Formats map[string]struct {
				Disk struct {
					Location string `json:"location"`
				} `json:"disk"`
			} `json:"formats"`
		} `json:"artifacts"`
	} `json:"architectures"`
}

// coreOSArch is the architecture key easyshift targets in the stream JSON.
const coreOSArch = "x86_64"

// SNOInstallationDisk is the in-VM device where bootstrap-in-place writes
// RHCOS. easyshift attaches every VM disk via virtio (see libvirt.go), so
// /dev/vda is the only primary disk present at install time.
const SNOInstallationDisk = "/dev/vda"

// installConfigTemplate produces install-config.yaml for SNO (single-node
// bootstrap-in-place). bootstrapInPlace.installationDisk is required by
// `openshift-install create single-node-ignition-config`; omitting it fails
// with "bootstrapInPlace: Required value".
const installConfigTemplate = `apiVersion: v1
baseDomain: {{.Cluster.Domain}}
compute:
- hyperthreading: Enabled
  name: worker
  replicas: {{.Cluster.WorkerCount}}
controlPlane:
  hyperthreading: Enabled
  name: master
  replicas: {{.Cluster.MasterCount}}
metadata:
  name: {{.Cluster.Name}}
networking:
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
{{- if .Cluster.MachineCIDR}}
  machineNetwork:
  - cidr: {{.Cluster.MachineCIDR}}
{{- end}}
  networkType: OVNKubernetes
  serviceNetwork:
  - 172.30.0.0/16
platform:
  none: {}
bootstrapInPlace:
  installationDisk: {{.InstallationDisk}}
pullSecret: '{{.PullSecret}}'
sshKey: '{{.SSHPublicKey}}'
`

// OpenShiftInstaller implements Installer by invoking openshift-install and
// coreos-installer through a CommandRunner. The struct holds no per-cluster
// state; binary paths come in via InstallerSpec on each call.
type OpenShiftInstaller struct {
	cmd interfaces.CommandRunner
}

// NewOpenShiftInstaller returns an Installer that shells out via cmd.
func NewOpenShiftInstaller(cmd interfaces.CommandRunner) *OpenShiftInstaller {
	return &OpenShiftInstaller{cmd: cmd}
}

// installConfigData is the input to installConfigTemplate. It wraps
// InstallerSpec with template-only fields (like InstallationDisk) so the
// interface type stays free of rendering details.
type installConfigData struct {
	interfaces.InstallerSpec
	InstallationDisk string
}

// WriteInstallConfig renders install-config.yaml into spec.ClusterDir.
func (i *OpenShiftInstaller) WriteInstallConfig(_ context.Context, spec interfaces.InstallerSpec) error {
	tmpl, err := template.New("install-config").Parse(installConfigTemplate)
	if err != nil {
		return fmt.Errorf("parse install-config template: %w", err)
	}
	path := filepath.Join(spec.ClusterDir, "install-config.yaml")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create install-config: %w", err)
	}
	defer func() { _ = f.Close() }()
	data := installConfigData{
		InstallerSpec:    spec,
		InstallationDisk: SNOInstallationDisk,
	}
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("render install-config: %w", err)
	}
	return nil
}

// CreateIgnitionConfigs runs `openshift-install create ignition-configs`.
func (i *OpenShiftInstaller) CreateIgnitionConfigs(ctx context.Context, spec interfaces.InstallerSpec) error {
	if _, err := i.cmd.Run(ctx, spec.InstallerPath, "create", "manifests", "--dir", spec.ClusterDir); err != nil {
		return fmt.Errorf("create manifests: %w", err)
	}
	if _, err := i.cmd.Run(ctx, spec.InstallerPath, "create", "ignition-configs", "--dir", spec.ClusterDir); err != nil {
		return fmt.Errorf("create ignition-configs: %w", err)
	}
	return nil
}

// CreateSingleNodeIgnition runs `openshift-install create single-node-ignition-config`.
func (i *OpenShiftInstaller) CreateSingleNodeIgnition(ctx context.Context, spec interfaces.InstallerSpec) error {
	if _, err := i.cmd.Run(ctx, spec.InstallerPath, "create", "single-node-ignition-config", "--dir", spec.ClusterDir); err != nil {
		return fmt.Errorf("create single-node-ignition-config: %w", err)
	}
	return nil
}

// EmbedIgnitionInISO runs `coreos-installer iso ignition embed`.
func (i *OpenShiftInstaller) EmbedIgnitionInISO(ctx context.Context, spec interfaces.InstallerSpec, isoPath, ignitionPath, outputPath string) error {
	if _, err := i.cmd.Run(ctx, spec.CoreOSInstallerPath,
		"iso", "ignition", "embed",
		"-i", ignitionPath,
		"-o", outputPath,
		isoPath,
	); err != nil {
		return fmt.Errorf("embed ignition in ISO: %w", err)
	}
	return nil
}

// EmbedNetworkKeyfileInISO runs `coreos-installer iso network embed`, planting
// a NetworkManager keyfile in the live ISO so the node configures its NIC
// statically from first boot. -f overwrites any existing network settings so
// the call is safe on a resumed build.
func (i *OpenShiftInstaller) EmbedNetworkKeyfileInISO(ctx context.Context, spec interfaces.InstallerSpec, keyfilePath, isoPath string) error {
	if _, err := i.cmd.Run(ctx, spec.CoreOSInstallerPath,
		"iso", "network", "embed",
		"-k", keyfilePath,
		"-f",
		isoPath,
	); err != nil {
		return fmt.Errorf("embed network keyfile in ISO: %w", err)
	}
	return nil
}

// WaitForInstallComplete blocks until install completes.
func (i *OpenShiftInstaller) WaitForInstallComplete(ctx context.Context, spec interfaces.InstallerSpec) error {
	out, errOut := installerWriters(spec)
	return i.cmd.RunStreaming(ctx, out, errOut,
		spec.InstallerPath, "wait-for", "install-complete", "--dir", spec.ClusterDir, "--log-level", "info")
}

// installerWriters returns the writers to pass to RunStreaming. If spec.Out
// is set, both stdout and stderr go to it (so the caller can MultiWriter to
// terminal + log). Otherwise defaults to os.Stdout/Stderr.
func installerWriters(spec interfaces.InstallerSpec) (stdout, stderr io.Writer) {
	if spec.Out != nil {
		return spec.Out, spec.Out
	}
	return os.Stdout, os.Stderr
}

// CoreOSLiveISOURL runs `openshift-install coreos print-stream-json` and
// returns the live ISO download URL for the targeted architecture. stdout is
// captured separately from stderr so installer log lines don't corrupt the
// JSON.
func (i *OpenShiftInstaller) CoreOSLiveISOURL(ctx context.Context, spec interfaces.InstallerSpec) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := i.cmd.RunStreaming(ctx, &stdout, &stderr, spec.InstallerPath, "coreos", "print-stream-json"); err != nil {
		return "", fmt.Errorf("coreos print-stream-json: %w: %s", err, stderr.String())
	}
	return parseCoreOSLiveISO(stdout.Bytes(), coreOSArch)
}

func parseCoreOSLiveISO(data []byte, arch string) (string, error) {
	var s coreOSStream
	if err := json.Unmarshal(data, &s); err != nil {
		return "", fmt.Errorf("parse stream json: %w", err)
	}
	a, ok := s.Architectures[arch]
	if !ok {
		return "", fmt.Errorf("stream json has no architecture %q", arch)
	}
	metal, ok := a.Artifacts["metal"]
	if !ok {
		return "", fmt.Errorf("stream json arch %q has no metal artifact", arch)
	}
	iso, ok := metal.Formats["iso"]
	if !ok {
		return "", fmt.Errorf("stream json arch %q metal has no iso format", arch)
	}
	if iso.Disk.Location == "" {
		return "", fmt.Errorf("stream json arch %q iso location is empty", arch)
	}
	return iso.Disk.Location, nil
}

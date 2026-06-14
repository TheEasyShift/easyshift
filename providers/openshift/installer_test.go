package openshift_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/openshift"
)

// streamJSON mirrors the shape of `openshift-install coreos print-stream-json`.
const streamJSON = `{
  "stream": "rhcos-4.21",
  "architectures": {
    "x86_64": {
      "artifacts": {
        "metal": {
          "release": "421.94.0",
          "formats": {
            "iso": {
              "disk": {
                "location": "https://rhcos.mirror.openshift.com/art/storage/prod/streams/4.21/builds/421.94.0/x86_64/rhcos-421.94.0-live.x86_64.iso",
                "sha256": "abc123"
              }
            },
            "pxe": {
              "kernel": {"location": "https://example/kernel"},
              "initramfs": {"location": "https://example/initramfs"},
              "rootfs": {"location": "https://example/rootfs"}
            }
          }
        }
      }
    }
  }
}`

// TestCoreOSLiveISOURL_ParsesStreamJSON drives the real OpenShiftInstaller
// with a fake CommandRunner whose RunStreaming emits canned stream JSON, and
// asserts the live ISO location is extracted.
func TestCoreOSLiveISOURL_ParsesStreamJSON(t *testing.T) {
	cmd := &fakes.CommandRunner{StreamStdout: []byte(streamJSON)}
	installer := openshift.NewOpenShiftInstaller(cmd)

	url, err := installer.CoreOSLiveISOURL(context.Background(), interfaces.InstallerSpec{
		InstallerPath: "/bin/openshift-install",
	})
	if err != nil {
		t.Fatalf("CoreOSLiveISOURL: %v", err)
	}
	want := "https://rhcos.mirror.openshift.com/art/storage/prod/streams/4.21/builds/421.94.0/x86_64/rhcos-421.94.0-live.x86_64.iso"
	if url != want {
		t.Errorf("live ISO url:\n  got  %q\n  want %q", url, want)
	}
}

func TestCoreOSLivePXEURLs_ParsesStreamJSON(t *testing.T) {
	cmd := &fakes.CommandRunner{StreamStdout: []byte(streamJSON)}
	installer := openshift.NewOpenShiftInstaller(cmd)

	pxe, err := installer.CoreOSLivePXEURLs(context.Background(), interfaces.InstallerSpec{
		InstallerPath: "/bin/openshift-install",
		Arch:          "x86_64",
	})
	if err != nil {
		t.Fatalf("CoreOSLivePXEURLs: %v", err)
	}
	if pxe.KernelURL != "https://example/kernel" || pxe.InitramfsURL != "https://example/initramfs" || pxe.RootfsURL != "https://example/rootfs" {
		t.Errorf("unexpected PXE urls: %+v", pxe)
	}
}

// TestWriteInstallConfig_RendersBootstrapInPlace pins the SNO-specific
// fields that `openshift-install create single-node-ignition-config`
// requires: a bootstrapInPlace.installationDisk pointing at the virtio
// primary disk, plus the right replica counts.
//
// Regression coverage for "bootstrapInPlace: Required value".
func TestWriteInstallConfig_RendersBootstrapInPlace(t *testing.T) {
	dir := t.TempDir()
	cmd := &fakes.CommandRunner{}
	installer := openshift.NewOpenShiftInstaller(cmd)

	spec := interfaces.InstallerSpec{
		ClusterDir: dir,
		Cluster: &config.ClusterConfig{
			Name:        "demo",
			Domain:      "local",
			MasterCount: 1,
			WorkerCount: 0,
			MachineCIDR: "192.168.1.0/24",
		},
		PullSecret:   `{"auths":{"fake":{"auth":"fake"}}}`,
		SSHPublicKey: "ssh-rsa AAAAFAKE",
	}
	if err := installer.WriteInstallConfig(context.Background(), spec); err != nil {
		t.Fatalf("WriteInstallConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "install-config.yaml"))
	if err != nil {
		t.Fatalf("read install-config: %v", err)
	}
	got := string(data)

	for _, want := range []string{
		"bootstrapInPlace:",
		"installationDisk: " + openshift.SNOInstallationDisk,
		"controlPlane:",
		"replicas: 1",
		"replicas: 0",
		"baseDomain: local",
		"name: demo",
		"machineNetwork:",
		"cidr: 192.168.1.0/24",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered install-config missing %q\n--- file ---\n%s", want, got)
		}
	}
}

// TestEmbedNetworkKeyfileInISO_RunsCoreOSInstaller asserts the exact
// coreos-installer subcommand and flags used to embed the static-network
// keyfile, including -f so resumed builds can overwrite prior settings.
func TestEmbedNetworkKeyfileInISO_RunsCoreOSInstaller(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	installer := openshift.NewOpenShiftInstaller(cmd)
	spec := interfaces.InstallerSpec{CoreOSInstallerPath: "/usr/bin/coreos-installer"}

	if err := installer.EmbedNetworkKeyfileInISO(context.Background(), spec,
		"/clusters/dr1/master.nmconnection", "/clusters/dr1/master.iso"); err != nil {
		t.Fatalf("EmbedNetworkKeyfileInISO: %v", err)
	}
	if len(cmd.Calls) != 1 {
		t.Fatalf("expected 1 command, got %d: %+v", len(cmd.Calls), cmd.Calls)
	}
	got := cmd.Calls[0]
	if got.Name != "/usr/bin/coreos-installer" {
		t.Errorf("binary = %q, want coreos-installer", got.Name)
	}
	args := strings.Join(got.Args, " ")
	for _, want := range []string{
		"iso network embed",
		"-k /clusters/dr1/master.nmconnection",
		"-f",
		"/clusters/dr1/master.iso",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
}

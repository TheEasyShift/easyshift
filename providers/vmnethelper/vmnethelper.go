// Package vmnethelper implements interfaces.NetworkProvisioner on macOS and
// builds the per-VM vmnet-helper sidecar invocation. vmnet-helper is a
// privileged, per-VM process (it requires --fd/--socket), not a standalone
// daemon, so the shared 192.168.126.0/24 network materializes when each VM's
// sidecar starts in shared mode with the same subnet. Per-cluster IPs are
// pinned via the ignition static keyfile, so AddHost/RemoveHost only track
// allocation.
package vmnethelper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Ensure the macOS provider satisfies the stage-facing network contract.
var _ interfaces.NetworkProvisioner = (*NetworkProvisioner)(nil)

// candidateBinaries are the known install locations for vmnet-helper, which is
// NOT on PATH: Homebrew puts it under the formula's libexec; upstream
// install.sh uses /opt/vmnet-helper/bin.
func candidateBinaries() []string {
	paths := []string{"/opt/vmnet-helper/bin/vmnet-helper"}
	if out, err := exec.Command("brew", "--prefix", "vmnet-helper").Output(); err == nil {
		prefix := filepath.Clean(string(trimNL(out)))
		paths = append([]string{filepath.Join(prefix, "libexec", "vmnet-helper")}, paths...)
	}
	return paths
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// ResolveBinary returns the absolute path to an installed vmnet-helper, or an
// error naming where it looked so preflight can guide the user.
func ResolveBinary() (string, error) {
	cands := candidateBinaries()
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("vmnet-helper not found (looked in %v); install with `brew install vmnet-helper`", cands)
}

// SidecarArgv builds the per-VM vmnet-helper argument list: shared mode on our
// subnet (gateway = subnet.1), bound to socketPath. The privileged spawn wraps
// these args with `sudo --non-interactive --close-from N` (Phase B).
func SidecarArgv(socketPath, subnet string) []string {
	return []string{
		"--socket", socketPath,
		"--operation-mode", "shared",
		"--start-address", subnet + ".1",
		"--end-address", subnet + ".254",
		"--subnet-mask", "255.255.255.0",
		// NOTE: vmnet-helper's --enable-tso / --enable-checksum-offload are NOT
		// usable with vfkit. They require a virtio-net header on the socket
		// frames to carry segmentation/checksum metadata (how QEMU uses them),
		// but Apple's VZ file-handle network attachment exchanges raw Ethernet
		// frames with no such header. Enabling them corrupts all payload traffic
		// (ARP still resolves, but ICMP/TCP get 100% loss). Verified 2026-06-15.
		// See the vfkit-boot-spike doc for the real lever (vmnet-broker, macOS 26+).
	}
}

// NetworkProvisioner is the stage-facing network contract. On macOS its methods
// are bookkeeping/validation; the real network work happens per-VM via the
// sidecar (started by the vfkit VMManager using SidecarArgv).
type NetworkProvisioner struct {
	cmd interfaces.CommandRunner
}

// NewNetworkProvisioner returns the macOS NetworkProvisioner.
func NewNetworkProvisioner(cmd interfaces.CommandRunner) *NetworkProvisioner {
	return &NetworkProvisioner{cmd: cmd}
}

// EnsureNetwork validates the shared-network identity. It does NOT shell out:
// the shared network comes up with the first per-VM sidecar (see package doc).
func (p *NetworkProvisioner) EnsureNetwork(_ context.Context, spec interfaces.NetworkSpec) error {
	if spec.Subnet == "" {
		return fmt.Errorf("vmnethelper: empty subnet in NetworkSpec")
	}
	return nil
}

// AddHost / RemoveHost track GlobalState allocation; IP pinning is via the
// ignition keyfile, so there is no vmnet mutation here.
func (p *NetworkProvisioner) AddHost(_ context.Context, _ string, _ interfaces.DHCPHost) error {
	return nil
}

func (p *NetworkProvisioner) RemoveHost(_ context.Context, _ string, _ interfaces.DHCPHost) error {
	return nil
}

// InspectNetwork reports what the provider knows. Reservations/leases live in
// GlobalState + the ignition keyfile, not the vmnet layer.
func (p *NetworkProvisioner) InspectNetwork(_ context.Context, _ string) (interfaces.NetworkInfo, error) {
	return interfaces.NetworkInfo{Exists: true}, nil
}

// ResetNetwork is a no-op: there is no persistent network definition to tear down.
func (p *NetworkProvisioner) ResetNetwork(_ context.Context, _ string) error { return nil }

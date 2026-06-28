package vmnethelper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Ensure the provider can launch per-VM sidecars.
var _ interfaces.SidecarLauncher = (*NetworkProvisioner)(nil)

// sidecarSocketWait is how long StartSidecar waits for vmnet-helper to bind the
// socket before giving up.
var sidecarSocketWait = 10 * time.Second

// StartSidecar spawns a per-VM vmnet-helper bound to socketPath, in shared mode
// on the easyshift subnet, under passwordless sudo. It returns once the socket
// exists (so the VM can attach) plus a stop func that terminates the helper.
// The helper is detached (its own process); vfkit attaches via
// `--device virtio-net,unixSocketPath=<socketPath>`.
func (p *NetworkProvisioner) StartSidecar(ctx context.Context, name, socketPath string) (func(), error) {
	bin, err := ResolveBinary()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, fmt.Errorf("sidecar %s: mkdir socket dir: %w", name, err)
	}
	// Reap any vmnet-helper a prior run leaked onto this socket before binding a
	// fresh one (also clears the stale socket file). Without this, a leaked
	// helper keeps its vmnet bridge port and the new helper stacks on top.
	reapSidecar(socketPath)

	// sudo --non-interactive <bin> --socket <path> --operation-mode shared ...
	args := append([]string{"--non-interactive", bin}, SidecarArgv(socketPath, config.BaseNetworkRange)...)
	cmd := exec.Command("sudo", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sidecar %s: start vmnet-helper: %w", name, err)
	}
	stop := func() {
		if cmd.Process != nil {
			// vmnet-helper runs as root (via sudo); SIGTERM the sudo child.
			_ = exec.Command("sudo", "--non-interactive", "kill", fmt.Sprintf("%d", cmd.Process.Pid)).Run()
			_ = cmd.Process.Kill()
		}
		_ = os.Remove(socketPath)
	}

	// Wait for the helper to bind the socket.
	deadline := time.Now().Add(sidecarSocketWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return stop, nil
		}
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	stop()
	return nil, fmt.Errorf("sidecar %s: vmnet-helper did not bind %s within %s", name, socketPath, sidecarSocketWait)
}

// StopSidecar reaps the per-VM vmnet-helper by its socket path. It is the
// cross-process fallback the vfkit supervisor calls on stop/delete: every CLI
// invocation after the create process exits has no in-memory stop handle, so it
// relies on this to terminate the detached helper. Idempotent — no matching
// helper is success.
func (p *NetworkProvisioner) StopSidecar(_ context.Context, _ string, socketPath string) error {
	reapSidecar(socketPath)
	return nil
}

// reapSidecar terminates any vmnet-helper (the sudo parent and its
// privilege-dropped child) still bound to socketPath, then removes the socket.
// It matches on the VM-unique socket path, so it only ever reaps this VM's
// helper — other clusters' sidecars on the shared network are untouched — and
// works without any in-memory state, which is what makes cross-process cleanup
// (and start-time stray cleanup) possible.
func reapSidecar(socketPath string) {
	if out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output(); err == nil {
		pids := matchingSidecarPIDs(string(out), socketPath)
		if len(pids) > 0 {
			// The sudo parent runs as root; one `sudo kill` reaps both it and
			// the privilege-dropped child.
			args := []string{"--non-interactive", "kill"}
			for _, pid := range pids {
				args = append(args, strconv.Itoa(pid))
			}
			_ = exec.Command("sudo", args...).Run()
		}
	}
	_ = os.Remove(socketPath)
}

// matchingSidecarPIDs returns the PIDs in `ps`-style output (one "pid command"
// line each) that name a vmnet-helper bound to socketPath. It requires BOTH the
// "vmnet-helper" token and the socket path so it never matches vfkit (which
// carries the same socket in its NIC arg but is not vmnet-helper), a helper for
// a different VM's socket, or the ps scan itself. Order is preserved.
func matchingSidecarPIDs(psOutput, socketPath string) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "vmnet-helper") || !strings.Contains(line, socketPath) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

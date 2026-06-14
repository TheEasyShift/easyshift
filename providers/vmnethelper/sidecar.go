package vmnethelper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	_ = os.Remove(socketPath) // clear any stale socket

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

package host

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// SystemHostInspector is the real HostInspector backed by the running OS.
// OS-specific probes (CPU virtualization, bridge inspection, ARP) live in
// host_linux.go / host_darwin.go; the cross-platform helpers stay here.
type SystemHostInspector struct{}

// NewSystemHostInspector returns a HostInspector that reads the actual host.
func NewSystemHostInspector() *SystemHostInspector { return &SystemHostInspector{} }

// LookPath returns nil if name resolves via exec.LookPath, else an error
// that names the binary so preflight output is obvious.
func (SystemHostInspector) LookPath(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required binary %q not found on PATH: %w", name, err)
	}
	return nil
}

// AvailableDiskBytes returns the bytes free on the filesystem holding path.
// path must exist; callers should pass a directory known to be present
// (e.g., the configDir, not an as-yet-uncreated cluster directory).
func (SystemHostInspector) AvailableDiskBytes(path string) (uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(s.Bavail) * uint64(s.Bsize), nil
}

// DialTCP returns nil if a TCP connect to addr succeeds within timeout.
func (SystemHostInspector) DialTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

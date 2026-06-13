// Package vfkit implements interfaces.VMManager on macOS by shelling out to
// vfkit (Apple Virtualization.framework). Unlike libvirtd, vfkit is one
// process per running VM, so this manager is a process supervisor: it
// persists a launch spec per VM and tracks the running pid.
package vfkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Ensure the vfkit VMManager satisfies the interface (catches drift if the
// interface grows new methods, e.g. ImportDisk).
var _ interfaces.VMManager = (*VMManager)(nil)

// VMManager supervises vfkit processes. stateDir holds one subdir per VM with
// its launch spec and pid file.
type VMManager struct {
	cmd      interfaces.CommandRunner
	stateDir string
}

// NewVMManager returns a vfkit-backed VMManager. stateDir is a writable dir
// (e.g. <configDir>/vfkit) where per-VM specs and pid files live.
func NewVMManager(cmd interfaces.CommandRunner, stateDir string) *VMManager {
	return &VMManager{cmd: cmd, stateDir: stateDir}
}

type launchSpec struct {
	Spec     interfaces.VMSpec
	DiskPath string
}

func (m *VMManager) vmDir(name string) string    { return filepath.Join(m.stateDir, name) }
func (m *VMManager) specPath(name string) string { return filepath.Join(m.vmDir(name), "spec.json") }
func (m *VMManager) pidPath(name string) string  { return filepath.Join(m.vmDir(name), "vfkit.pid") }
func (m *VMManager) restPath(name string) string { return filepath.Join(m.vmDir(name), "rest.sock") }

// Create persists the launch spec and pre-allocates the disk image path. It
// does not start vfkit (Start does), mirroring libvirt's define/start split.
func (m *VMManager) Create(_ context.Context, spec interfaces.VMSpec) error {
	if err := os.MkdirAll(m.vmDir(spec.Name), 0o755); err != nil {
		return fmt.Errorf("vfkit: mkdir vm dir: %w", err)
	}
	ls := launchSpec{Spec: spec, DiskPath: filepath.Join(m.vmDir(spec.Name), "disk.img")}
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("vfkit: marshal spec: %w", err)
	}
	return os.WriteFile(m.specPath(spec.Name), data, 0o644)
}

// Start launches vfkit for the named VM. The production CommandRunner runs the
// process detached and records its pid; the fake records the argv for tests.
func (m *VMManager) Start(ctx context.Context, name string) error {
	ls, err := m.load(name)
	if err != nil {
		return err
	}
	args := m.buildArgs(name, ls)
	if _, err := m.cmd.Run(ctx, "vfkit", args...); err != nil {
		return fmt.Errorf("vfkit start %s: %w", name, err)
	}
	return nil
}

// buildArgs assembles the vfkit command line from the launch spec.
func (m *VMManager) buildArgs(name string, ls launchSpec) []string {
	s := ls.Spec
	args := []string{
		"--cpus", strconv.Itoa(s.VCPUs),
		"--memory", strconv.Itoa(s.MemoryMiB),
		"--device", "virtio-blk,path=" + ls.DiskPath,
		"--device", "virtio-net," + s.NetworkArg + ",mac=" + s.MAC,
		"--device", "rosetta,mountTag=rosetta",
		"--restful-uri", "unix://" + m.restPath(name),
	}
	if s.KernelArgs != "" {
		args = append(args, "--bootloader",
			"linux,kernel="+filepath.Join(m.vmDir(name), "vmlinuz")+
				",initrd="+filepath.Join(m.vmDir(name), "initrd.img")+
				",cmdline=\""+s.KernelArgs+"\"")
	}
	return args
}

// IsRunning reports whether the recorded pid is alive.
func (m *VMManager) IsRunning(_ context.Context, name string) (bool, error) {
	pid, err := m.readPID(name)
	if err != nil {
		return false, nil // no pid file → not started
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	return proc.Signal(syscall.Signal(0)) == nil, nil
}

// Stop signals the vfkit process to terminate.
func (m *VMManager) Stop(_ context.Context, name string) error {
	pid, err := m.readPID(name)
	if err != nil {
		return nil // nothing to stop
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)
	return nil
}

// Delete stops the VM and removes its state dir (spec, pid, disk).
func (m *VMManager) Delete(ctx context.Context, name string) error {
	_ = m.Stop(ctx, name)
	return os.RemoveAll(m.vmDir(name))
}

// CheckAccess verifies vfkit is usable. The presence check is done in preflight
// via HostInspector.LookPath; here we report success.
func (m *VMManager) CheckAccess(_ context.Context) error { return nil }

// ImportISO / ImportDisk / RemoveISO / StoragePoolActive are libvirt
// storage-pool concepts with no vfkit analog (boot uses PXE assets served over
// HTTP). No-ops.
func (m *VMManager) ImportISO(_ context.Context, _, _, _ string) (string, error)  { return "", nil }
func (m *VMManager) ImportDisk(_ context.Context, _, _, _ string) (string, error) { return "", nil }
func (m *VMManager) RemoveISO(_ context.Context, _, _ string) error               { return nil }
func (m *VMManager) StoragePoolActive(_ context.Context, _ string) error          { return nil }

func (m *VMManager) load(name string) (launchSpec, error) {
	var ls launchSpec
	data, err := os.ReadFile(m.specPath(name))
	if err != nil {
		return ls, fmt.Errorf("vfkit: read spec %s: %w", name, err)
	}
	return ls, json.Unmarshal(data, &ls)
}

func (m *VMManager) readPID(name string) (int, error) {
	data, err := os.ReadFile(m.pidPath(name))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

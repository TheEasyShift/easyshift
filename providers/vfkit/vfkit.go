// Package vfkit implements interfaces.VMManager on macOS by supervising vfkit
// (Apple Virtualization.framework) processes. Unlike libvirtd, vfkit is one
// process per running VM, so this manager persists per-VM state and tracks the
// running pid via vfkit's --pidfile.
//
// Bootstrap-in-place needs two boot phases, because RHCOS ignition can only be
// delivered over the network on macOS (no coreos-installer to embed it), which
// needs a kernel cmdline (--bootloader linux) — but that bootloader can't boot
// the installed disk after the reboot. So:
//
//	install phase: --bootloader linux (live kernel/initramfs + ignition.config.url
//	               + rootfs url) installs RHCOS to the disk, then the guest stops.
//	run phase:     --bootloader efi + the disk only boots the installed system.
//
// The supervisor switches phases: the first time the install-phase VM stops
// (install complete), Start transitions to the run phase. The phase is
// persisted so the watchdog and `easyshift start` resume the right one.
package vfkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/TheEasyShift/easyshift/interfaces"
)

var _ interfaces.VMManager = (*VMManager)(nil)

const (
	phaseInstall = "install"
	phaseRun     = "run"
)

// VMManager supervises vfkit processes. stateDir holds one subdir per VM.
type VMManager struct {
	stateDir string
	sidecar  interfaces.SidecarLauncher
}

// NewVMManager returns a vfkit-backed VMManager. stateDir is a writable dir
// (e.g. <configDir>/vfkit); sidecar starts the per-VM vmnet-helper network.
func NewVMManager(stateDir string, sidecar interfaces.SidecarLauncher) *VMManager {
	return &VMManager{stateDir: stateDir, sidecar: sidecar}
}

type launchSpec struct {
	Spec     interfaces.VMSpec
	DiskPath string
}

func (m *VMManager) vmDir(name string) string     { return filepath.Join(m.stateDir, name) }
func (m *VMManager) specPath(name string) string  { return filepath.Join(m.vmDir(name), "spec.json") }
func (m *VMManager) pidPath(name string) string   { return filepath.Join(m.vmDir(name), "vfkit.pid") }
func (m *VMManager) phasePath(name string) string { return filepath.Join(m.vmDir(name), "phase") }
func (m *VMManager) sockPath(name string) string  { return filepath.Join(m.vmDir(name), "net.sock") }
func (m *VMManager) efiPath(name string) string   { return filepath.Join(m.vmDir(name), "efistore") }
func (m *VMManager) consolePath(name string) string {
	return filepath.Join(m.vmDir(name), "console.log")
}
func (m *VMManager) launchLogPath(name string) string {
	return filepath.Join(m.vmDir(name), "vfkit-launch.log")
}

// Create persists the launch spec, pre-allocates the disk, marks the VM for the
// install phase, and starts it (matching libvirt's create-and-run semantics).
func (m *VMManager) Create(ctx context.Context, spec interfaces.VMSpec) error {
	if err := os.MkdirAll(m.vmDir(spec.Name), 0o755); err != nil {
		return fmt.Errorf("vfkit: mkdir vm dir: %w", err)
	}
	ls := launchSpec{Spec: spec, DiskPath: filepath.Join(m.vmDir(spec.Name), "disk.img")}
	if err := m.createDisk(ls.DiskPath, spec.DiskSizeGiB); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("vfkit: marshal spec: %w", err)
	}
	if err := os.WriteFile(m.specPath(spec.Name), data, 0o644); err != nil {
		return err
	}
	if err := m.setPhase(spec.Name, phaseInstall); err != nil {
		return err
	}
	return m.launch(ctx, spec.Name)
}

// Start (re)launches the VM. Called by `easyshift start` and the install
// watchdog. The first call after the install-phase VM stops transitions to the
// run phase (the disk is now installed); subsequent calls relaunch the current
// phase.
func (m *VMManager) Start(ctx context.Context, name string) error {
	if running, _ := m.IsRunning(ctx, name); running {
		return nil
	}
	if m.phase(name) == phaseInstall {
		if err := m.setPhase(name, phaseRun); err != nil {
			return err
		}
	}
	return m.launch(ctx, name)
}

// launch spawns vfkit detached for the VM's current phase, after starting its
// network sidecar.
func (m *VMManager) launch(ctx context.Context, name string) error {
	ls, err := m.load(name)
	if err != nil {
		return err
	}
	if m.sidecar != nil {
		if _, err := m.sidecar.StartSidecar(ctx, name, m.sockPath(name)); err != nil {
			return fmt.Errorf("vfkit %s: start network sidecar: %w", name, err)
		}
	}
	args := m.buildArgs(name, ls, m.phase(name))
	cmd := exec.Command("vfkit", args...)
	// Capture vfkit's own stdout/stderr (boot params, fatal errors) — distinct
	// from the guest serial console (console.log) — so launch failures are
	// diagnosable.
	if logf, err := os.OpenFile(m.launchLogPath(name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("vfkit %s: launch: %w", name, err)
	}
	// Detach: we track the VM via --pidfile, not this handle.
	_ = cmd.Process.Release()
	return nil
}

// buildArgs assembles the vfkit command line for the given phase. install uses
// the live kernel/initramfs + our cmdline; run uses EFI to boot the disk.
func (m *VMManager) buildArgs(name string, ls launchSpec, phase string) []string {
	s := ls.Spec
	args := []string{
		"--cpus", strconv.Itoa(s.VCPUs),
		"--memory", strconv.Itoa(s.MemoryMiB),
		"--device", "virtio-blk,path=" + ls.DiskPath,
		"--device", "virtio-net,unixSocketPath=" + m.sockPath(name) + ",mac=" + s.MAC,
		"--device", "rosetta,mountTag=rosetta",
		"--device", "virtio-serial,logFilePath=" + m.consolePath(name),
		"--pidfile", m.pidPath(name),
	}
	switch phase {
	case phaseRun:
		args = append(args, "--bootloader", "efi,variable-store="+m.efiPath(name)+",create")
	default: // install
		args = append(args,
			"--bootloader", strings.Join([]string{
				"linux",
				"kernel=" + s.KernelPath,
				"initrd=" + s.InitrdPath,
				"cmdline=" + strconv.Quote(s.KernelArgs),
			}, ","))
	}
	return args
}

// IsRunning reports whether the recorded vfkit pid is alive.
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

// Stop terminates the vfkit process and clears the network socket.
func (m *VMManager) Stop(_ context.Context, name string) error {
	if pid, err := m.readPID(name); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(m.sockPath(name))
	return nil
}

// Delete stops the VM and removes its state dir.
func (m *VMManager) Delete(ctx context.Context, name string) error {
	_ = m.Stop(ctx, name)
	return os.RemoveAll(m.vmDir(name))
}

// CheckAccess: vfkit presence is verified in preflight via LookPath.
func (m *VMManager) CheckAccess(_ context.Context) error { return nil }

// ImportISO / ImportDisk / RemoveISO / StoragePoolActive are libvirt
// storage-pool concepts with no vfkit analog (boot uses PXE assets over HTTP).
func (m *VMManager) ImportISO(_ context.Context, _, _, _ string) (string, error)  { return "", nil }
func (m *VMManager) ImportDisk(_ context.Context, _, _, _ string) (string, error) { return "", nil }
func (m *VMManager) RemoveISO(_ context.Context, _, _ string) error               { return nil }
func (m *VMManager) StoragePoolActive(_ context.Context, _ string) error          { return nil }

// createDisk creates a sparse raw disk image of sizeGiB if it doesn't exist.
func (m *VMManager) createDisk(path string, sizeGiB int) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if sizeGiB <= 0 {
		sizeGiB = 120
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("vfkit: create disk: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(int64(sizeGiB) * 1024 * 1024 * 1024); err != nil {
		return fmt.Errorf("vfkit: size disk: %w", err)
	}
	return nil
}

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
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (m *VMManager) phase(name string) string {
	data, err := os.ReadFile(m.phasePath(name))
	if err != nil {
		return phaseInstall
	}
	return strings.TrimSpace(string(data))
}

func (m *VMManager) setPhase(name, phase string) error {
	if err := os.MkdirAll(m.vmDir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m.phasePath(name), []byte(phase), 0o644)
}

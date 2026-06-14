package vfkit

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
)

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func hasArgContaining(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func newMgr(t *testing.T) *VMManager { return NewVMManager(t.TempDir(), nil) }

func installSpec() launchSpec {
	return launchSpec{
		Spec: interfaces.VMSpec{
			Name: "master-0-demo", MemoryMiB: 16000, VCPUs: 4, DiskSizeGiB: 120,
			MAC:        "52:54:00:11:22:33",
			KernelPath: "/cache/vmlinuz", InitrdPath: "/cache/initramfs.img",
			KernelArgs: "coreos.live.rootfs_url=http://10.0.0.1:9393/demo/rootfs.img ignition.config.url=http://10.0.0.1:9393/demo/config.ign",
		},
		DiskPath: "/state/master-0-demo/disk.img",
	}
}

func TestBuildArgs_InstallPhase(t *testing.T) {
	m := newMgr(t)
	args := m.buildArgs("master-0-demo", installSpec(), phaseInstall)
	joined := strings.Join(args, " ")
	if !contains(args, "--cpus") || !contains(args, "4") {
		t.Errorf("missing --cpus 4: %s", joined)
	}
	if !contains(args, "--memory") || !contains(args, "16000") {
		t.Errorf("missing --memory 16000: %s", joined)
	}
	if !hasArgContaining(args, "linux") || !hasArgContaining(args, "kernel=/cache/vmlinuz") || !hasArgContaining(args, "initrd=/cache/initramfs.img") {
		t.Errorf("install phase must use --bootloader linux with kernel/initrd: %s", joined)
	}
	if !hasArgContaining(args, "ignition.config.url=") {
		t.Errorf("install cmdline must carry ignition.config.url: %s", joined)
	}
	if !hasArgContaining(args, "virtio-net,unixSocketPath=") {
		t.Errorf("NIC must attach to the sidecar unix socket: %s", joined)
	}
	if !hasArgContaining(args, "--pidfile") {
		t.Errorf("missing --pidfile: %s", joined)
	}
	if hasArgContaining(args, "efi") {
		t.Errorf("install phase must not use EFI: %s", joined)
	}
}

func TestBuildArgs_RunPhase(t *testing.T) {
	m := newMgr(t)
	args := m.buildArgs("master-0-demo", installSpec(), phaseRun)
	joined := strings.Join(args, " ")
	if !hasArgContaining(args, "efi,variable-store=") {
		t.Errorf("run phase must boot via EFI: %s", joined)
	}
	if hasArgContaining(args, "kernel=") {
		t.Errorf("run phase must not pass a kernel (boots the installed disk): %s", joined)
	}
}

func TestPhaseRoundTrip(t *testing.T) {
	m := newMgr(t)
	if err := m.setPhase("vm", phaseInstall); err != nil {
		t.Fatalf("setPhase: %v", err)
	}
	if got := m.phase("vm"); got != phaseInstall {
		t.Errorf("phase = %q, want install", got)
	}
	if err := m.setPhase("vm", phaseRun); err != nil {
		t.Fatalf("setPhase: %v", err)
	}
	if got := m.phase("vm"); got != phaseRun {
		t.Errorf("phase = %q, want run", got)
	}
}

func TestIsRunning_FalseBeforeStart(t *testing.T) {
	m := newMgr(t)
	running, err := m.IsRunning(context.Background(), "nope")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running {
		t.Error("VM should not be running with no pid file")
	}
}

func TestISONoops(t *testing.T) {
	m := newMgr(t)
	if _, err := m.ImportISO(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("ImportISO no-op: %v", err)
	}
	if _, err := m.ImportDisk(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("ImportDisk no-op: %v", err)
	}
	if err := m.StoragePoolActive(context.Background(), "p"); err != nil {
		t.Errorf("StoragePoolActive no-op: %v", err)
	}
}

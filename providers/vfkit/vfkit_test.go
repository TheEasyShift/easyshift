package vfkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
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

func TestRebootDetected(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"single boot (install, no reboot)", "[ OK ] Reached target First Boot Complete.\nlocalhost login:\n", false},
		{"two boots (rebooted)", "Reached target First Boot Complete.\n...install...\nReached target First Boot Complete.\n", true},
		{"no boot banner yet", "[ OK ] Reached target Basic System.\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := rebootDetected([]byte(c.in)); got != c.want {
			t.Errorf("%s: rebootDetected = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestISONoops(t *testing.T) {
	m := newMgr(t)
	if _, err := m.ImportISO(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("ImportISO no-op: %v", err)
	}
	if err := m.StoragePoolActive(context.Background(), "p"); err != nil {
		t.Errorf("StoragePoolActive no-op: %v", err)
	}
}

func TestBuildArgs_ExtraDisks(t *testing.T) {
	m := newMgr(t)
	ls := installSpec()
	ls.DiskPath = "/d/disk.img"
	ls.Spec.ExtraDisks = []interfaces.ExtraDisk{{Path: "/cache/store.img", ReadOnly: true, Shareable: true}}
	for _, phase := range []string{phaseInstall, phaseRun} {
		args := m.buildArgs("master-0-demo", ls, phase)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "virtio-blk,path=/cache/store.img") {
			t.Errorf("%s phase: extra disk not attached: %v", phase, args)
		}
		// The store must come after the primary OS disk so the guest's
		// by-label mount is unambiguous and /dev/vda stays the install disk.
		if strings.Index(joined, "path=/d/disk.img") > strings.Index(joined, "path=/cache/store.img") {
			t.Errorf("%s phase: extra disk attached before primary: %v", phase, args)
		}
	}
}

func TestImportDiskAndDeleteCleanup(t *testing.T) {
	m := newMgr(t)
	src := filepath.Join(t.TempDir(), "store.img")
	if err := os.WriteFile(src, []byte("STORE"), 0o644); err != nil {
		t.Fatal(err)
	}
	vol := config.ImageStoreVolName("master-0-demo")
	got, err := m.ImportDisk(context.Background(), "ignored-pool", vol, src)
	if err != nil {
		t.Fatalf("ImportDisk: %v", err)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "STORE" {
		t.Fatalf("imported disk unreadable at %q: %v", got, err)
	}
	// Missing source must error clearly (the bake stage never produced it).
	if _, err := m.ImportDisk(context.Background(), "p", vol, src+".missing"); err == nil {
		t.Error("ImportDisk with missing source: expected error")
	}
	// Delete must remove the imported store copy along with the VM dir.
	if err := m.Delete(context.Background(), "master-0-demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("imported disk %q survived Delete", got)
	}
}

func TestCreateDataDiskAndDeleteCleanup(t *testing.T) {
	m := newMgr(t)
	vol := config.ODFVolName("master-0-demo")
	got, err := m.CreateDataDisk(context.Background(), "ignored-pool", vol, 2)
	if err != nil {
		t.Fatalf("CreateDataDisk: %v", err)
	}
	fi, err := os.Stat(got)
	if err != nil || fi.Size() != 2<<30 {
		t.Fatalf("data disk wrong: %v size=%d", err, fi.Size())
	}
	// Idempotent: second call reuses, does not truncate away content.
	if again, err := m.CreateDataDisk(context.Background(), "p", vol, 2); err != nil || again != got {
		t.Fatalf("CreateDataDisk not idempotent: %q vs %q (%v)", again, got, err)
	}
	if err := m.Delete(context.Background(), "master-0-demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("data disk %q survived Delete", got)
	}
}

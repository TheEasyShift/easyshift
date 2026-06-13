package vfkit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/vfkit"
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

func TestVFKit_StartArgs(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())

	if err := vm.Create(context.Background(), interfaces.VMSpec{
		Name:        "master-0-demo",
		MemoryMiB:   16000,
		VCPUs:       4,
		DiskSizeGiB: 120,
		MAC:         "52:54:00:11:22:33",
		NetworkArg:  "unixSocketPath=/tmp/vm.sock",
		KernelArgs:  "ignition.config.url=http://10.0.0.1:9393/demo/config.ign",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := vm.Start(context.Background(), "master-0-demo"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var start *fakes.CommandCall
	for i := range cmd.Calls {
		if cmd.Calls[i].Name == "vfkit" {
			start = &cmd.Calls[i]
		}
	}
	if start == nil {
		t.Fatal("expected a vfkit invocation")
	}
	joined := strings.Join(start.Args, " ")
	if !contains(start.Args, "--cpus") || !contains(start.Args, "4") {
		t.Errorf("missing --cpus 4: %s", joined)
	}
	if !contains(start.Args, "--memory") || !contains(start.Args, "16000") {
		t.Errorf("missing --memory 16000: %s", joined)
	}
	if !hasArgContaining(start.Args, "virtio-net") {
		t.Errorf("missing virtio-net device: %s", joined)
	}
	if !hasArgContaining(start.Args, "ignition.config.url=") {
		t.Errorf("kernel cmdline must carry ignition.config.url: %s", joined)
	}
	if !hasArgContaining(start.Args, "--restful-uri") {
		t.Errorf("missing --restful-uri for lifecycle control: %s", joined)
	}
}

func TestVFKit_IsRunning_FalseBeforeStart(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())
	if err := vm.Create(context.Background(), interfaces.VMSpec{Name: "m"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	running, err := vm.IsRunning(context.Background(), "m")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running {
		t.Error("VM should not be running before Start")
	}
}

func TestVFKit_ISONoops(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())
	if _, err := vm.ImportISO(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("ImportISO should be a no-op on vfkit: %v", err)
	}
	if err := vm.StoragePoolActive(context.Background(), "p"); err != nil {
		t.Errorf("StoragePoolActive should be a no-op on vfkit: %v", err)
	}
}

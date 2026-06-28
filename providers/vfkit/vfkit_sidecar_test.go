package vfkit

import (
	"context"
	"testing"
)

type fakeSidecar struct {
	startCalls  int
	stopSockets []string
}

func (f *fakeSidecar) StartSidecar(_ context.Context, _, socketPath string) (func(), error) {
	f.startCalls++
	return func() {}, nil
}

func (f *fakeSidecar) StopSidecar(_ context.Context, _, socketPath string) error {
	f.stopSockets = append(f.stopSockets, socketPath)
	return nil
}

// The orphan-reaping fix: Stop/Delete invoked in a fresh process (this
// VMManager never ran StartSidecar, so its in-memory stop map is empty) must
// still reap the detached sidecar via the cross-process StopSidecar fallback,
// targeting the VM's socket path.
func TestStopReapsSidecarCrossProcess(t *testing.T) {
	f := &fakeSidecar{}
	m := NewVMManager(t.TempDir(), f)

	if err := m.Stop(context.Background(), "master-0-dr1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.stopSockets) != 1 {
		t.Fatalf("expected exactly 1 StopSidecar call, got %d", len(f.stopSockets))
	}
	if want := m.sockPath("master-0-dr1"); f.stopSockets[0] != want {
		t.Fatalf("StopSidecar socket = %q, want %q", f.stopSockets[0], want)
	}
}

// A nil sidecar (Linux/libvirt wiring, and existing tests) must not panic.
func TestStopNilSidecarSafe(t *testing.T) {
	m := NewVMManager(t.TempDir(), nil)
	if err := m.Stop(context.Background(), "master-0-dr1"); err != nil {
		t.Fatalf("Stop with nil sidecar: %v", err)
	}
}

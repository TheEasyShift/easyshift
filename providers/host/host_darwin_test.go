//go:build darwin

package host

import "testing"

func TestDarwin_HasCPUVirtualization_AppleSilicon(t *testing.T) {
	ok, err := SystemHostInspector{}.HasCPUVirtualization()
	if err != nil {
		t.Fatalf("HasCPUVirtualization: %v", err)
	}
	if !ok {
		t.Error("expected CPU virtualization available on this Apple Silicon host")
	}
}

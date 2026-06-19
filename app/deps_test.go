package app_test

import (
	"context"
	"testing"

	"github.com/TheEasyShift/easyshift/app"
	"github.com/TheEasyShift/easyshift/config"
)

func TestNewDarwinDeps_WiresMacProviders(t *testing.T) {
	cfg := config.NewDefaultConfig(t.TempDir())
	deps, err := app.NewDarwinDeps(cfg, "10.0.0.1")
	if err != nil {
		t.Fatalf("NewDarwinDeps: %v", err)
	}
	if deps.VM == nil || deps.Net == nil {
		t.Fatal("darwin deps must wire VM and Net")
	}
	if deps.Installer == nil || deps.ImageBaker == nil || deps.Files == nil {
		t.Fatal("darwin deps must wire the shared deps (installer, image baker, files)")
	}
	// The vfkit VMManager treats ImportISO as a no-op (libvirt would shell out).
	if _, err := deps.VM.ImportISO(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("expected vfkit ImportISO no-op, got %v", err)
	}
}

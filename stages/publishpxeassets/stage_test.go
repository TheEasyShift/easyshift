package publishpxeassets_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/stages/publishpxeassets"
)

func TestName(t *testing.T) {
	if got := publishpxeassets.New(nil, nil).Name(); got != "publish-pxe-assets" {
		t.Errorf("unexpected stage name %q", got)
	}
}

func TestKernelCmdline(t *testing.T) {
	cmdline := publishpxeassets.KernelCmdline("http://10.0.0.1:9393", "demo")
	if !strings.Contains(cmdline, "ignition.config.url=http://10.0.0.1:9393/demo/config.ign") {
		t.Errorf("cmdline missing ignition url: %q", cmdline)
	}
	if !strings.Contains(cmdline, "coreos.live.rootfs_url=http://10.0.0.1:9393/demo/rootfs.img") {
		t.Errorf("cmdline missing rootfs url: %q", cmdline)
	}
}

// TestApplyMergesImageStoreWhenBaking asserts the served ignition gets the
// baked-store wiring (the macOS equivalent of embed-ignition-iso's live-ISO
// merge on Linux).
func TestApplyMergesImageStoreWhenBaking(t *testing.T) {
	cfgDir := t.TempDir()
	files := &fakes.FileServer{Root: t.TempDir(), URL: "http://fake:9393"}
	inst := &fakes.Installer{}
	s := publishpxeassets.New(files, inst)

	cfg := &config.Config{ConfigDir: cfgDir}
	c := &config.ClusterConfig{
		Name: "t", NetworkMode: config.NetworkModeNAT,
		BakeImages:   true,
		IPAddresses:  []string{"192.168.126.9"},
		MACAddresses: []string{"52:54:00:00:00:09"},
		MasterCount:  1,
	}
	sc := &interfaces.StageContext{Config: cfg, Cluster: c}
	if err := os.MkdirAll(sc.ClusterDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sc.RHCOSRootfsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sc.RHCOSRootfsPath(), []byte("ROOTFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sc.ClusterDir(), "bootstrap-in-place-for-live-iso.ign"), []byte(`{"ignition":{"version":"3.4.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !inst.MergedImageStoreIgnition {
		t.Error("expected MergeImageStoreIntoLiveISOIgnition on the served ignition")
	}

	// And not when baking is off.
	inst2 := &fakes.Installer{}
	c.BakeImages = false
	if err := publishpxeassets.New(files, inst2).Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply (no bake): %v", err)
	}
	if inst2.MergedImageStoreIgnition {
		t.Error("merge must not run without --bake-images")
	}
}

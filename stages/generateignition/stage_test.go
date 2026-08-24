package generateignition_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/stages/generateignition"
)

// TestApplyWritesRosettaManifestOnDarwin asserts the darwin install path drops
// the Rosetta MachineConfig before generating the SNO ignition, so the
// installed node can run amd64 binaries from first boot.
func TestApplyWritesRosettaManifestOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("rosetta manifest is only written on darwin hosts")
	}
	cfgDir := t.TempDir()
	cfg := &config.Config{ConfigDir: cfgDir}
	c := &config.ClusterConfig{Name: "t", NetworkMode: config.NetworkModeNAT}
	sc := &interfaces.StageContext{Config: cfg, Cluster: c}
	if err := os.MkdirAll(sc.ClusterDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.PullSecretPath(cfgDir), []byte(`{"auths":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sc.ClusterDir(), "id_rsa.pub"), []byte("ssh-rsa AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}

	inst := &fakes.Installer{}
	s := generateignition.New(inst, &fakes.DNSResolver{}, &fakes.HostInspector{})
	if err := s.Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !inst.WroteRosettaManifest {
		t.Error("expected WriteRosettaManifest to be called on darwin")
	}
}

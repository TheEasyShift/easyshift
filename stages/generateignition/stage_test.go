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
	c := &config.ClusterConfig{Name: "t", NetworkMode: config.NetworkModeNAT, BakeImages: true}
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
	// openshift-install only renders pre-dropped openshift/ manifests into the
	// ignition when `create manifests` ran first (validated on hardware —
	// without it the extra MachineConfigs are silently ignored). So the order
	// must be: CreateManifests, then the manifest writes, then the ignition.
	assertOrder(t, inst.Sequence, "CreateManifests", "WriteRosettaManifest", "CreateSingleNodeIgnition")
	assertOrder(t, inst.Sequence, "CreateManifests", "WriteImageStoreManifest", "CreateSingleNodeIgnition")
}

func assertOrder(t *testing.T, seq []string, want ...string) {
	t.Helper()
	i := 0
	for _, s := range seq {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("call order %v does not contain %v in order", seq, want)
	}
}

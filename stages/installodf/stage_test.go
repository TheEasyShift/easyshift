package installodf_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/stages/installodf"
)

// newSC builds a StageContext with a real config dir (needed because
// ClusterDir/OCBinaryPath derive from it) and the given cluster.
func newSC(t *testing.T, cluster *config.ClusterConfig) *interfaces.StageContext {
	t.Helper()
	dir := t.TempDir()
	return &interfaces.StageContext{
		Cluster: cluster,
		Config:  &config.Config{ConfigDir: dir},
	}
}

// TestApply_ODFEnabled drives all four ODF phases in order and derives the
// spec from the cluster (bake-images occupies /dev/vdb, so ODF gets /dev/vdc).
func TestApply_ODFEnabled(t *testing.T) {
	odf := &fakes.ODFInstaller{}
	stage := installodf.New(odf)
	cluster := &config.ClusterConfig{
		Name:       "t",
		ODF:        true,
		BakeImages: true,
		ODFDiskGB:  100,
		OCPVersion: "4.22.9",
	}
	sc := newSC(t, cluster)

	if err := stage.Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantSeq := []string{"InstallOperators", "SetupLVM", "EnableSingleNode", "CreateStorageCluster"}
	if got := odf.Sequence; !equalStrings(got, wantSeq) {
		t.Fatalf("Sequence: got %v want %v", got, wantSeq)
	}
	if got := odf.LastSpec.DevicePath; got != "/dev/vdc" {
		t.Errorf("DevicePath: got %q want /dev/vdc", got)
	}
	if got := odf.LastSpec.Channel; got != "stable-4.22" {
		t.Errorf("Channel: got %q want stable-4.22", got)
	}
	if got := odf.LastSpec.DataPVCSizeGi; got != 30 {
		t.Errorf("DataPVCSizeGi: got %d want 30", got)
	}
	if got := odf.LastSpec.KubeconfigPath; !strings.HasSuffix(got, "clusters/t/auth/kubeconfig") {
		t.Errorf("KubeconfigPath: got %q, want suffix clusters/t/auth/kubeconfig", got)
	}
	if got, want := odf.LastSpec.WorkDir, sc.ClusterDir(); got != want {
		t.Errorf("WorkDir: got %q want %q", got, want)
	}
}

// TestApply_ODFDisabled confirms the stage is a no-op when the cluster did
// not opt into --odf.
func TestApply_ODFDisabled(t *testing.T) {
	odf := &fakes.ODFInstaller{}
	stage := installodf.New(odf)
	cluster := &config.ClusterConfig{
		Name:       "t",
		ODF:        false,
		BakeImages: true,
		ODFDiskGB:  100,
		OCPVersion: "4.22.9",
	}
	sc := newSC(t, cluster)

	if err := stage.Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := odf.Sequence; len(got) != 0 {
		t.Fatalf("Sequence: got %v want empty", got)
	}
}

// TestApply_NoBakeImages confirms the ODF data disk falls back to /dev/vdb
// when the bake store isn't occupying it.
func TestApply_NoBakeImages(t *testing.T) {
	odf := &fakes.ODFInstaller{}
	stage := installodf.New(odf)
	cluster := &config.ClusterConfig{
		Name:       "t",
		ODF:        true,
		BakeImages: false,
		ODFDiskGB:  100,
		OCPVersion: "4.22.9",
	}
	sc := newSC(t, cluster)

	if err := stage.Apply(context.Background(), sc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := odf.LastSpec.DevicePath; got != "/dev/vdb" {
		t.Errorf("DevicePath: got %q want /dev/vdb", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

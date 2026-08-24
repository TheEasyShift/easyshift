package openshift_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/openshift"
)

func TestRenderRosettaMachineConfig(t *testing.T) {
	mc := openshift.RenderRosettaMachineConfig()
	for _, want := range []string{
		"kind: MachineConfig",
		"machineconfiguration.openshift.io/role: master",
		"name: " + openshift.RosettaMachineConfigName,
		"run-rosetta.mount",
		"rosetta-binfmt.service",
		"Type=virtiofs",
		"Options=context=system_u:object_r:container_file_t:s0",
		"/proc/sys/fs/binfmt_misc/register",
		"/run/rosetta/rosetta",
	} {
		if !strings.Contains(mc, want) {
			t.Errorf("rosetta MachineConfig missing %q:\n%s", want, mc)
		}
	}
}

func TestWriteRosettaManifest(t *testing.T) {
	dir := t.TempDir()
	inst := openshift.NewOpenShiftInstaller(&fakes.CommandRunner{})
	if err := inst.WriteRosettaManifest(context.Background(), interfaces.InstallerSpec{ClusterDir: dir}); err != nil {
		t.Fatalf("WriteRosettaManifest: %v", err)
	}
	path := filepath.Join(dir, "openshift", openshift.RosettaMachineConfigName+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if !strings.Contains(string(data), "rosetta-binfmt.service") {
		t.Errorf("manifest content missing rosetta unit:\n%s", data)
	}
}

package odf

import (
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
)

func TestChannelForVersion(t *testing.T) {
	// Implemented in config (stages cannot import providers); tested here
	// beside its main consumer.
	if got := config.OLMChannelForVersion("4.22.9"); got != "stable-4.22" {
		t.Errorf("channel: %q", got)
	}
}

func TestRenderStorageClusterSingleNodeDeltas(t *testing.T) {
	mc := RenderStorageCluster(30)
	// The three hardware-validated 4.22 deltas from the spec's spike results:
	for _, want := range []string{
		"count: 3", "replica: 1", // replica: 3 re-raises minNodes past SINGLE_NODE
		"preparePlacement:",                   // empty/absent placements render an invalid TSC
		"topologyKey: kubernetes.io/hostname", // the no-op TSC
		"whenUnsatisfiable: ScheduleAnyway",
		"storage: 30Gi",
		"storageClassName: " + ImmediateStorageClassName,
		"reconcileStrategy: ignore",
		"volumeMode: Block",
	} {
		if !strings.Contains(mc, want) {
			t.Errorf("StorageCluster missing %q", want)
		}
	}
	if strings.Contains(mc, "replica: 3") {
		t.Error("replica: 3 defeats SINGLE_NODE's node-count relaxation on ODF 4.22")
	}
}

func TestRenderDriverFloorsAllContainers(t *testing.T) {
	d := RenderDriver("openshift-storage.rbd.csi.ceph.com")
	for _, want := range []string{"replicas: 1", "plugin:", "omapGenerator:", "addons:", "attacher:", "provisioner:", "resizer:", "snapshotter:"} {
		if !strings.Contains(d, want) {
			t.Errorf("Driver missing %q (default plugin alone is 250Mi)", want)
		}
	}
}

func TestRenderImmediateStorageClass(t *testing.T) {
	sc := RenderImmediateStorageClass()
	if !strings.Contains(sc, "volumeBindingMode: Immediate") || !strings.Contains(sc, "topolvm.io") {
		t.Errorf("SC not immediate/topolvm:\n%s", sc)
	}
}

func TestRenderOperatorsAndLVM(t *testing.T) {
	ops := RenderOperators("stable-4.22")
	for _, want := range []string{"openshift-lvm-storage", "openshift-storage", "lvms-operator", "odf-operator", "channel: stable-4.22", "redhat-operators"} {
		if !strings.Contains(ops, want) {
			t.Errorf("operators missing %q", want)
		}
	}
	lvm := RenderLVMCluster("/dev/vdc")
	if !strings.Contains(lvm, "/dev/vdc") || !strings.Contains(lvm, "thinPoolConfig") {
		t.Errorf("LVMCluster wrong:\n%s", lvm)
	}
}

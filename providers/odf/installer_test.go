package odf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
)

// canned wires a fakes.CommandRunner.RunFunc that answers each of the
// jsonpath/get probes InstallOperators..CreateStorageCluster issue, so every
// poll converges on its first check. Keyed loosely on substrings present in
// the args, per the task brief's suggested fake shape.
func canned(subLongName string) *fakes.CommandRunner {
	return &fakes.CommandRunner{
		RunFunc: func(_ string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case contains(args, "csv"):
				// Both the lvms-operator and ocs-operator* CSV lookups list
				// CSVs in a namespace and get parsed client-side for a
				// name-prefix match, so one canned multi-line answer covers
				// both InstallOperators checks.
				return []byte("lvms-operator.v4.22.0=Succeeded\nocs-operator.v4.22.2=Succeeded\n"), nil
			case contains(args, "crd"):
				return []byte(""), nil
			case contains(args, "lvmcluster"):
				return []byte("Ready"), nil
			case contains(args, "sub") && strings.Contains(joined, "jsonpath"):
				return []byte(subLongName), nil
			case contains(args, "deploy") && strings.Contains(joined, "SINGLE_NODE"):
				return []byte("true"), nil
			case contains(args, "deploy") && strings.Contains(joined, "Available"):
				return []byte("True"), nil
			case contains(args, "storagecluster") && strings.Contains(joined, "jsonpath"):
				return []byte("Ready"), nil
			case contains(args, "sc"):
				return []byte(""), nil
			default:
				return []byte(""), nil
			}
		},
	}
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func testSpec(t *testing.T) interfaces.ODFSpec {
	t.Helper()
	dir := t.TempDir()
	return interfaces.ODFSpec{
		KubeconfigPath: filepath.Join(dir, "kubeconfig"),
		OCBinaryPath:   filepath.Join(dir, "oc"),
		WorkDir:        dir,
		Channel:        "stable-4.22",
		DevicePath:     "/dev/vdc",
		DataPVCSizeGi:  30,
	}
}

// withShortTimeouts shrinks every poll timeout for the duration of a test so
// timeout-path tests don't actually wait minutes, then restores them.
func withShortTimeouts(t *testing.T) {
	t.Helper()
	origCSV, origLVM, origSettle, origSC, origSClasses, origPoll :=
		waitCSV, waitLVM, waitSettle, waitStorageCluster, waitStorageClasses, pollInterval
	waitCSV = 30 * time.Millisecond
	waitLVM = 30 * time.Millisecond
	waitSettle = 30 * time.Millisecond
	waitStorageCluster = 30 * time.Millisecond
	waitStorageClasses = 30 * time.Millisecond
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		waitCSV, waitLVM, waitSettle, waitStorageCluster, waitStorageClasses, pollInterval =
			origCSV, origLVM, origSettle, origSC, origSClasses, origPoll
	})
}

func assertOCCall(t *testing.T, cmd *fakes.CommandRunner, spec interfaces.ODFSpec) {
	t.Helper()
	if len(cmd.Calls) == 0 {
		t.Fatal("expected at least one command call")
	}
	for _, c := range cmd.Calls {
		if c.Name != spec.OCBinaryPath {
			t.Errorf("call used binary %q, want %q", c.Name, spec.OCBinaryPath)
		}
		if len(c.Args) < 2 || c.Args[0] != "--kubeconfig" || c.Args[1] != spec.KubeconfigPath {
			t.Errorf("call %v missing --kubeconfig %s", c.Args, spec.KubeconfigPath)
		}
	}
}

func TestInstallOperators(t *testing.T) {
	withShortTimeouts(t)
	spec := testSpec(t)
	cmd := canned("")
	i := New(cmd)

	if err := i.InstallOperators(context.Background(), spec); err != nil {
		t.Fatalf("InstallOperators: %v", err)
	}
	assertOCCall(t, cmd, spec)

	entries, err := os.ReadDir(filepath.Join(spec.WorkDir, "odf"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected rendered manifests under %s/odf, got %v (err %v)", spec.WorkDir, entries, err)
	}
	found := false
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(spec.WorkDir, "odf", e.Name()))
		if strings.Contains(string(b), "lvms-operator") && strings.Contains(string(b), "odf-operator") {
			found = true
		}
	}
	if !found {
		t.Error("expected a rendered file containing both operator subscriptions")
	}
}

func TestSetupLVM(t *testing.T) {
	withShortTimeouts(t)
	spec := testSpec(t)
	cmd := canned("")
	i := New(cmd)

	if err := i.SetupLVM(context.Background(), spec); err != nil {
		t.Fatalf("SetupLVM: %v", err)
	}
	assertOCCall(t, cmd, spec)

	entries, err := os.ReadDir(filepath.Join(spec.WorkDir, "odf"))
	if err != nil {
		t.Fatalf("read odf dir: %v", err)
	}
	var gotLVMCluster, gotImmediateSC bool
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(spec.WorkDir, "odf", e.Name()))
		if strings.Contains(string(b), "/dev/vdc") {
			gotLVMCluster = true
		}
		if strings.Contains(string(b), ImmediateStorageClassName) {
			gotImmediateSC = true
		}
	}
	if !gotLVMCluster {
		t.Error("expected a rendered LVMCluster manifest with the device path")
	}
	if !gotImmediateSC {
		t.Error("expected a rendered immediate StorageClass manifest")
	}
}

func TestEnableSingleNodePatchesSubByDiscoveredName(t *testing.T) {
	withShortTimeouts(t)
	spec := testSpec(t)
	const longSubName = "odf-operator-ocs-operator-abc123"
	cmd := canned(longSubName)
	i := New(cmd)

	if err := i.EnableSingleNode(context.Background(), spec); err != nil {
		t.Fatalf("EnableSingleNode: %v", err)
	}
	assertOCCall(t, cmd, spec)

	var patched bool
	for _, c := range cmd.Calls {
		if len(c.Args) > 0 && contains(c.Args, "patch") && contains(c.Args, longSubName) {
			patched = true
		}
	}
	if !patched {
		t.Errorf("expected a patch call naming sub %q, calls: %+v", longSubName, cmd.Calls)
	}

	entries, err := os.ReadDir(filepath.Join(spec.WorkDir, "odf"))
	if err != nil {
		t.Fatalf("read odf dir: %v", err)
	}
	var gotRBD, gotCephFS, gotMonitoring bool
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(spec.WorkDir, "odf", e.Name()))
		if strings.Contains(string(b), "openshift-storage.rbd.csi.ceph.com") {
			gotRBD = true
		}
		if strings.Contains(string(b), "openshift-storage.cephfs.csi.ceph.com") {
			gotCephFS = true
		}
		if strings.Contains(string(b), "cluster-monitoring-config") {
			gotMonitoring = true
		}
	}
	if !gotRBD || !gotCephFS {
		t.Errorf("expected both Driver CRs rendered, rbd=%v cephfs=%v", gotRBD, gotCephFS)
	}
	if !gotMonitoring {
		t.Error("expected the monitoring trim ConfigMap rendered")
	}

	var labeled bool
	for _, c := range cmd.Calls {
		if contains(c.Args, "label") && contains(c.Args, "nodes") {
			labeled = true
		}
	}
	if !labeled {
		t.Error("expected an `oc label nodes` call")
	}
}

func TestCreateStorageCluster(t *testing.T) {
	withShortTimeouts(t)
	spec := testSpec(t)
	cmd := canned("")
	i := New(cmd)

	if err := i.CreateStorageCluster(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageCluster: %v", err)
	}
	assertOCCall(t, cmd, spec)

	entries, err := os.ReadDir(filepath.Join(spec.WorkDir, "odf"))
	if err != nil {
		t.Fatalf("read odf dir: %v", err)
	}
	var gotSC bool
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(spec.WorkDir, "odf", e.Name()))
		if strings.Contains(string(b), "storage: 30Gi") {
			gotSC = true
		}
	}
	if !gotSC {
		t.Error("expected the rendered StorageCluster manifest with the requested PVC size")
	}

	var gotBothSCChecks bool
	rbd, cephfs := false, false
	for _, c := range cmd.Calls {
		if contains(c.Args, "sc") && contains(c.Args, RBDStorageClass) {
			rbd = true
		}
		if contains(c.Args, "sc") && contains(c.Args, CephFSStorageClass) {
			cephfs = true
		}
	}
	gotBothSCChecks = rbd && cephfs
	if !gotBothSCChecks {
		t.Errorf("expected checks for both StorageClasses, rbd=%v cephfs=%v", rbd, cephfs)
	}
}

// TestPollUntilTimeoutNamesPhase pins the timeout-path contract directly: a
// probe that never converges must return promptly (within the shrunk
// timeout) with an error naming the phase, not hang or loop forever.
func TestPollUntilTimeoutNamesPhase(t *testing.T) {
	withShortTimeouts(t)
	i := New(&fakes.CommandRunner{})

	calls := 0
	err := i.pollUntil(context.Background(), waitCSV, "the widget phase", func() (bool, error) {
		calls++
		return false, nil
	})
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "the widget phase") {
		t.Errorf("timeout error %q does not name the phase", err.Error())
	}
	if calls < 2 {
		t.Errorf("expected the probe to be polled more than once before timing out, got %d calls", calls)
	}
}

// TestPollUntilRetriesTransientProbeErrors confirms a probe error (an API
// flap) is retried rather than failing the poll immediately.
func TestPollUntilRetriesTransientProbeErrors(t *testing.T) {
	withShortTimeouts(t)
	i := New(&fakes.CommandRunner{})

	calls := 0
	err := i.pollUntil(context.Background(), 200*time.Millisecond, "transient phase", func() (bool, error) {
		calls++
		if calls < 3 {
			return false, errors.New("connection refused")
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("pollUntil: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 probe calls, got %d", calls)
	}
}

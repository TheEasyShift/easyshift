package vmnethelper_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/vmnethelper"
)

func hasArgContaining(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// SidecarArgv must produce a shared-mode, our-subnet, socket-bound invocation.
func TestSidecarArgv(t *testing.T) {
	args := vmnethelper.SidecarArgv("/tmp/vm.sock", "192.168.126")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--operation-mode", "shared", "--start-address", "192.168.126.1", "--subnet-mask", "255.255.255.0"} {
		if !hasArgContaining(args, want) {
			t.Errorf("SidecarArgv missing %q: %s", want, joined)
		}
	}
	if !hasArgContaining(args, "/tmp/vm.sock") {
		t.Errorf("SidecarArgv must bind the socket path: %s", joined)
	}
}

// ResolveBinary returns an error (not a bogus path) when vmnet-helper is absent,
// so preflight can produce an actionable message. On a machine with it installed
// it returns an existing path.
func TestResolveBinary_Behavior(t *testing.T) {
	path, err := vmnethelper.ResolveBinary()
	if err != nil {
		if path != "" {
			t.Errorf("on error, path must be empty, got %q", path)
		}
		return // not installed on this runner (e.g. Linux CI) — acceptable
	}
	if !strings.Contains(path, "vmnet-helper") {
		t.Errorf("resolved path does not look like vmnet-helper: %q", path)
	}
}

// EnsureNetwork is bookkeeping on macOS: it does NOT shell out (the network
// comes up with the first per-VM sidecar). It must not invoke vmnet-helper.
func TestEnsureNetwork_NoShellOut(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	p := vmnethelper.NewNetworkProvisioner(cmd)
	if err := p.EnsureNetwork(context.Background(), interfaces.NetworkSpec{Name: "easyshift-nat", Subnet: "192.168.126"}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	for _, c := range cmd.Calls {
		if c.Name == "vmnet-helper" {
			t.Errorf("EnsureNetwork must not spawn vmnet-helper directly (it is per-VM): %+v", c)
		}
	}
}

func TestAddRemoveHost_NoError(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	p := vmnethelper.NewNetworkProvisioner(cmd)
	h := interfaces.DHCPHost{MAC: "52:54:00:11:22:33", IP: "192.168.126.10", Hostname: "master-0-demo"}
	if err := p.AddHost(context.Background(), "easyshift-nat", h); err != nil {
		t.Errorf("AddHost: %v", err)
	}
	if err := p.RemoveHost(context.Background(), "easyshift-nat", h); err != nil {
		t.Errorf("RemoveHost: %v", err)
	}
}

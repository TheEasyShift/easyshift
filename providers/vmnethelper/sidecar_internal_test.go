package vmnethelper

import "testing"

// matchingSidecarPIDs must select exactly the vmnet-helper processes (sudo
// parent + privilege-dropped child) bound to THIS VM's socket, and nothing
// else: not vfkit (which also carries the socket path in its NIC arg but is not
// vmnet-helper), not a helper for a different VM's socket, not the ps scan
// itself. This is the cross-process orphan-reaping selector.
func TestMatchingSidecarPIDs(t *testing.T) {
	sock := "/Users/x/.config/easyshift/vfkit/master-0-dr1/net.sock"
	other := "/Users/x/.config/easyshift/vfkit/master-0-other/net.sock"
	ps := "" +
		"  46235 sudo --non-interactive /opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper --socket " + sock + " --operation-mode shared\n" +
		"  46236 /opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper --socket " + sock + " --operation-mode shared --start-address 192.168.126.1\n" +
		"  77001 vfkit --cpus 4 --device virtio-net,unixSocketPath=" + sock + ",mac=52:54:00:fd:4d:60\n" +
		"  88002 /opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper --socket " + other + " --operation-mode shared\n" +
		"  99003 ps -axww -o pid=,command=\n"

	got := matchingSidecarPIDs(ps, sock)
	want := []int{46235, 46236}
	if len(got) != len(want) {
		t.Fatalf("matchingSidecarPIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matchingSidecarPIDs = %v, want %v", got, want)
		}
	}
}

func TestMatchingSidecarPIDsNoMatch(t *testing.T) {
	if got := matchingSidecarPIDs("  12345 some other process\n", "/no/such/sock"); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

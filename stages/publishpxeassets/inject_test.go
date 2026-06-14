package publishpxeassets

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/config"
)

func TestInjectStaticNetwork_PreservesAndAdds(t *testing.T) {
	c := &config.ClusterConfig{
		Name:         "dr1",
		MACAddresses: []string{"52:54:00:11:22:33"},
		IPAddresses:  []string{"192.168.126.5"},
	}
	src := []byte(`{"ignition":{"version":"3.4.0"},"storage":{"files":[{"path":"/etc/existing"}]}}`)

	out, err := injectStaticNetwork(src, c)
	if err != nil {
		t.Fatalf("injectStaticNetwork: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	// ignition version preserved.
	if ig, _ := m["ignition"].(map[string]any); ig["version"] != "3.4.0" {
		t.Errorf("ignition version not preserved: %v", m["ignition"])
	}
	files := m["storage"].(map[string]any)["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("expected existing + keyfile = 2 files, got %d", len(files))
	}
	// The keyfile entry pins the allocated IP/MAC.
	last := files[1].(map[string]any)
	if !strings.Contains(last["path"].(string), "master-0-dr1.nmconnection") {
		t.Errorf("keyfile path wrong: %v", last["path"])
	}
	src2 := last["contents"].(map[string]any)["source"].(string)
	if !strings.HasPrefix(src2, "data:;base64,") {
		t.Errorf("keyfile contents not a base64 data url: %q", src2)
	}
}

func TestNATKeyfile_HasStaticIP(t *testing.T) {
	c := &config.ClusterConfig{Name: "dr1", MACAddresses: []string{"52:54:00:aa:bb:cc"}, IPAddresses: []string{"192.168.126.5"}}
	kf := natKeyfile(c)
	for _, want := range []string{"method=manual", "address1=192.168.126.5/24,192.168.126.1", "52:54:00:AA:BB:CC", "dns=192.168.126.1;"} {
		if !strings.Contains(kf, want) {
			t.Errorf("keyfile missing %q:\n%s", want, kf)
		}
	}
}

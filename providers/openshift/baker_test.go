package openshift

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseImageURL(t *testing.T) {
	got := ReleaseImageURL("4.21.0", "x86_64")
	want := "quay.io/openshift-release-dev/ocp-release:4.21.0-x86_64"
	if got != want {
		t.Fatalf("ReleaseImageURL: got %q want %q", got, want)
	}
	if got := ReleaseImageURL("4.21.0", "aarch64"); !strings.HasSuffix(got, "-aarch64") {
		t.Fatalf("aarch64 URL missing arch suffix: %q", got)
	}
}

func TestParseReleasePullspecs(t *testing.T) {
	in := []byte(`{
	  "references": {
	    "spec": {
	      "tags": [
	        {"name": "cli", "from": {"kind": "DockerImage", "name": "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:aaa"}},
	        {"name": "etcd", "from": {"kind": "DockerImage", "name": "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:bbb"}},
	        {"name": "broken", "from": {"kind": "DockerImage", "name": ""}}
	      ]
	    }
	  }
	}`)
	got, err := parseReleasePullspecs(in)
	if err != nil {
		t.Fatalf("parseReleasePullspecs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pullspecs (empty skipped), got %d: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "sha256:aaa") || !strings.HasSuffix(got[1], "sha256:bbb") {
		t.Fatalf("unexpected pullspecs: %v", got)
	}
}

func TestSystemdEscapePath(t *testing.T) {
	// Matches `systemd-escape -p /var/lib/baked-images`: '/' -> '-', '-' -> \x2d.
	if got, want := systemdEscapePath("/var/lib/baked-images"), `var-lib-baked\x2dimages`; got != want {
		t.Fatalf("systemdEscapePath: got %q want %q", got, want)
	}
	if got := systemdEscapePath("/"); got != "-" {
		t.Fatalf("root path: got %q want %q", got, "-")
	}
}

func TestRenderMountUnit(t *testing.T) {
	name, contents := RenderMountUnit()
	if name != `var-lib-baked\x2dimages.mount` {
		t.Fatalf("mount unit name: got %q", name)
	}
	for _, want := range []string{
		"What=/dev/disk/by-label/baked-images",
		"Where=/var/lib/baked-images",
		"Options=ro,nofail",
		"Before=crio.service",
		"RequiredBy=crio.service",
	} {
		if !strings.Contains(contents, want) {
			t.Errorf("mount unit missing %q\n%s", want, contents)
		}
	}
}

func TestRenderStorageConfDropin(t *testing.T) {
	got := RenderStorageConfDropin()
	if !strings.Contains(got, "additionalimagestores") || !strings.Contains(got, "/var/lib/baked-images") {
		t.Fatalf("storage.conf drop-in missing key/path:\n%s", got)
	}
}

func TestRenderMachineConfig(t *testing.T) {
	mc := RenderMachineConfig()
	for _, want := range []string{
		"kind: MachineConfig",
		"machineconfiguration.openshift.io/role: master",
		MachineConfigName,
		"/etc/containers/storage.conf.d/10-baked-images.conf",
		"data:text/plain;base64,",
		`var-lib-baked\x2dimages.mount`,
	} {
		if !strings.Contains(mc, want) {
			t.Errorf("MachineConfig missing %q", want)
		}
	}
}

func TestMergeBakedStoreIntoIgnition(t *testing.T) {
	// A minimal ignition with a pre-existing file + unit, to prove we append.
	base := []byte(`{
	  "ignition": {"version": "3.2.0"},
	  "storage": {"files": [{"path": "/etc/existing"}]},
	  "systemd": {"units": [{"name": "existing.service"}]}
	}`)
	out, err := MergeBakedStoreIntoIgnition(base)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var cfg struct {
		Storage struct {
			Files []struct {
				Path     string `json:"path"`
				Contents struct {
					Source string `json:"source"`
				} `json:"contents"`
			} `json:"files"`
		} `json:"storage"`
		Systemd struct {
			Units []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			} `json:"units"`
		} `json:"systemd"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("unmarshal merged: %v\n%s", err, out)
	}
	if len(cfg.Storage.Files) != 2 {
		t.Fatalf("want 2 files (existing + dropin), got %d", len(cfg.Storage.Files))
	}
	if len(cfg.Systemd.Units) != 2 {
		t.Fatalf("want 2 units (existing + mount), got %d", len(cfg.Systemd.Units))
	}
	var foundDropin, foundMount bool
	for _, f := range cfg.Storage.Files {
		if f.Path == storageConfDropinPath {
			foundDropin = true
			if !strings.HasPrefix(f.Contents.Source, "data:text/plain;base64,") {
				t.Errorf("dropin source not a data URL: %q", f.Contents.Source)
			}
		}
	}
	for _, u := range cfg.Systemd.Units {
		if u.Name == `var-lib-baked\x2dimages.mount` {
			foundMount = true
			if !u.Enabled {
				t.Errorf("mount unit not enabled")
			}
		}
	}
	if !foundDropin || !foundMount {
		t.Fatalf("merged ignition missing dropin=%t mount=%t", foundDropin, foundMount)
	}
}

func TestMergeBakedStoreIntoIgnition_EmptyConfig(t *testing.T) {
	// No storage/systemd keys present: merge must create them.
	out, err := MergeBakedStoreIntoIgnition([]byte(`{"ignition":{"version":"3.2.0"}}`))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The unit name's backslash is JSON-escaped in raw output; assert on the
	// dropin path and the mount unit body, which survive verbatim.
	if !strings.Contains(string(out), storageConfDropinPath) ||
		!strings.Contains(string(out), "Where=/var/lib/baked-images") {
		t.Fatalf("merge into empty config dropped entries:\n%s", out)
	}
}

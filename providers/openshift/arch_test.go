package openshift_test

import (
	"testing"

	"github.com/TheEasyShift/easyshift/providers/openshift"
)

func TestHostClientPlatform(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "mac-arm64"},
		{"darwin", "amd64", "mac"},
		{"linux", "amd64", "linux"},
		{"linux", "arm64", "linux"},
	}
	for _, c := range cases {
		if got := openshift.HostClientPlatform(c.goos, c.goarch); got != c.want {
			t.Errorf("HostClientPlatform(%q,%q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestOCPClientURL_ArchAware(t *testing.T) {
	got := openshift.OCPClientURL("aarch64", "4.21.0", "openshift-install-mac-arm64.tar.gz")
	want := "https://mirror.openshift.com/pub/openshift-v4/aarch64/clients/ocp/4.21.0/openshift-install-mac-arm64.tar.gz"
	if got != want {
		t.Errorf("OCPClientURL = %q, want %q", got, want)
	}
}

func TestClientTarballs(t *testing.T) {
	if got := openshift.InstallClientTarball("mac-arm64"); got != "openshift-install-mac-arm64.tar.gz" {
		t.Errorf("InstallClientTarball = %q", got)
	}
	if got := openshift.OCClientTarball("linux"); got != "openshift-client-linux.tar.gz" {
		t.Errorf("OCClientTarball = %q", got)
	}
}

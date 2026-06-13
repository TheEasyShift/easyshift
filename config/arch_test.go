package config_test

import (
	"testing"

	"github.com/TheEasyShift/easyshift/config"
)

func TestPayloadArch(t *testing.T) {
	cases := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}
	for goarch, want := range cases {
		if got := config.PayloadArch(goarch); got != want {
			t.Errorf("PayloadArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

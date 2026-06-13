package publishpxeassets_test

import (
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/stages/publishpxeassets"
)

func TestName(t *testing.T) {
	if got := publishpxeassets.New(nil).Name(); got != "publish-pxe-assets" {
		t.Errorf("unexpected stage name %q", got)
	}
}

func TestKernelCmdline(t *testing.T) {
	cmdline := publishpxeassets.KernelCmdline("http://10.0.0.1:9393", "demo")
	if !strings.Contains(cmdline, "ignition.config.url=http://10.0.0.1:9393/demo/config.ign") {
		t.Errorf("cmdline missing ignition url: %q", cmdline)
	}
	if !strings.Contains(cmdline, "coreos.live.rootfs_url=http://10.0.0.1:9393/demo/rootfs.img") {
		t.Errorf("cmdline missing rootfs url: %q", cmdline)
	}
}

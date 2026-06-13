package openshift_test

import (
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/providers/openshift"
)

func TestRosettaButaneFragment(t *testing.T) {
	frag := openshift.RosettaButaneFragment()
	for _, want := range []string{"rosetta", "binfmt_misc", "virtiofs", "/proc/sys/fs/binfmt_misc/register"} {
		if !strings.Contains(frag, want) {
			t.Errorf("rosetta fragment missing %q:\n%s", want, frag)
		}
	}
}

//go:build darwin

package host

import (
	"os/exec"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// HasCPUVirtualization reports whether the host can run Virtualization.framework
// guests. Every Apple Silicon Mac on a supported macOS can, so we confirm the
// hardware feature via sysctl `hw.optional.arm64` (1 on Apple Silicon).
func (SystemHostInspector) HasCPUVirtualization() (bool, error) {
	out, err := exec.Command("sysctl", "-n", "hw.optional.arm64").Output()
	if err != nil {
		return false, nil // not Apple Silicon (Intel Mac) → no native arm64 virt
	}
	return len(out) > 0 && out[0] == '1', nil
}

// InspectBridge is not used on macOS in this phase (bridge mode is deferred);
// report "not a bridge" so NAT-mode preflight is unaffected.
func (SystemHostInspector) InspectBridge(_ string) (interfaces.BridgeInfo, error) {
	return interfaces.BridgeInfo{Exists: false}, nil
}

// ARPLookup is not used on macOS in this phase (bridge-mode IP verification is
// deferred); return "" (no entry).
func (SystemHostInspector) ARPLookup(_ string) (string, error) {
	return "", nil
}

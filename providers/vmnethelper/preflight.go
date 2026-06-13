package vmnethelper

import (
	"context"
	"fmt"
)

// NetworkPreflight verifies vmnet-helper is installed and runnable under
// passwordless sudo, so the per-VM sidecar (spawned non-interactively at VM
// start) won't stall on a password prompt. It implements
// interfaces.NetworkPreflighter.
func (p *NetworkProvisioner) NetworkPreflight(ctx context.Context) error {
	bin, err := ResolveBinary()
	if err != nil {
		return err
	}
	// `sudo --non-interactive` fails (rather than prompting) when no valid
	// credential/NOPASSWD rule applies — exactly the condition that would later
	// hang the sidecar launch.
	if _, err := p.cmd.Run(ctx, "sudo", "--non-interactive", bin, "--version"); err != nil {
		return fmt.Errorf("vmnet-helper is not runnable under passwordless sudo: %w\n\n%s", err, PrivilegeHint(bin))
	}
	return nil
}

// PrivilegeHint returns the exact one-time command to install the vmnet-helper
// sudoers rule, with the NOPASSWD path pointed at the resolved binary (the
// shipped rule hardcodes /opt/vmnet-helper/bin, which the Homebrew libexec
// install does not match).
func PrivilegeHint(bin string) string {
	return "Install the vmnet-helper sudoers rule (one time):\n" +
		"  VH=$(brew --prefix vmnet-helper)\n" +
		"  sudo sh -c \"sed 's#/opt/vmnet-helper/bin/vmnet-helper#" + bin + "#' \\\n" +
		"    \\\"$VH/share/doc/vmnet-helper/sudoers.d/vmnet-helper\\\" > /etc/sudoers.d/vmnet-helper \\\n" +
		"    && chmod 0640 /etc/sudoers.d/vmnet-helper\"\n" +
		"Then re-run. (vmnet-helper opens the vmnet interface; easyshift never needs root itself.)"
}

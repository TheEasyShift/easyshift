package openshift

import (
	"fmt"
	"strings"
)

// RosettaMachineConfigName is the role-scoped MachineConfig that wires Apple
// Rosetta into the installed node. 99- prefix orders it after the rendered
// base config.
const RosettaMachineConfigName = "99-master-rosetta"

// rosettaMountUnit mounts the vfkit "rosetta" virtiofs share. The unit name
// must be the systemd path escape of Where= (/run/rosetta -> run-rosetta.mount).
//
// context= is load-bearing: the share carries no SELinux labels (unlabeled_t),
// which container_t cannot execute — an amd64 container entrypoint would die
// with SIGSEGV when the kernel maps the pre-opened translator at exec time.
// Labeling the whole share container_file_t at mount lets confined containers
// run it (validated on hardware; a plain host process is unconfined and works
// either way). It must be set on the FIRST mount of the boot: the kernel
// refuses a context that differs from the share's live superblock.
const rosettaMountUnit = `[Unit]
Description=Rosetta virtiofs share

[Mount]
What=rosetta
Where=/run/rosetta
Type=virtiofs
Options=context=system_u:object_r:container_file_t:s0

[Install]
WantedBy=local-fs.target
`

// rosettaBinfmtUnit registers the x86-64 binfmt_misc handler pointing at the
// mounted Rosetta translator. The \x escapes are passed through literally
// (single-quoted shell, echo without -e): binfmt_misc itself decodes them.
// Flags: O (open interpreter at register time), C (credentials of the binary),
// F (fix binary: pre-load the interpreter so it resolves inside containers
// whose rootfs has no /run/rosetta).
const rosettaBinfmtUnit = `[Unit]
Description=Register Rosetta binfmt_misc handler
Requires=run-rosetta.mount
After=run-rosetta.mount

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'echo ":rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/run/rosetta/rosetta:OCF" > /proc/sys/fs/binfmt_misc/register'

[Install]
WantedBy=multi-user.target
`

// RenderRosettaMachineConfig returns the master MachineConfig YAML that mounts
// the vfkit rosetta virtiofs share and registers the x86-64 binfmt_misc
// handler on the installed node. Dropped into the install dir's openshift/ so
// `create single-node-ignition-config` renders it into the node's ignition —
// amd64 binaries run translated from first boot.
func RenderRosettaMachineConfig() string {
	return fmt.Sprintf(`apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: %s
spec:
  config:
    ignition:
      version: 3.2.0
    systemd:
      units:
      - name: run-rosetta.mount
        enabled: true
        contents: |
%s
      - name: rosetta-binfmt.service
        enabled: true
        contents: |
%s
`, RosettaMachineConfigName, indentLines(rosettaMountUnit, "          "), indentLines(rosettaBinfmtUnit, "          "))
}

// indentLines prefixes every non-empty line of s with prefix (for embedding a
// multi-line unit file under a YAML block scalar).
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, ln := range lines {
		if ln != "" {
			lines[i] = prefix + ln
		}
	}
	return strings.Join(lines, "\n")
}

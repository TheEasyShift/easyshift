package openshift

// RosettaButaneFragment returns a Butane/MachineConfig snippet that mounts the
// vfkit "rosetta" virtiofs share and registers a binfmt_misc handler so the
// guest runs x86-64 ELF binaries via Apple Rosetta. Merged into the SNO
// ignition on macOS hosts. The on-hardware phase validates and, if needed,
// tunes the binfmt magic/mask against a live guest.
func RosettaButaneFragment() string {
	return `# Rosetta: mount the virtiofs share and register x86-64 binfmt_misc
systemd:
  units:
    - name: run-rosetta.mount
      enabled: true
      contents: |
        [Unit]
        Description=Rosetta virtiofs share
        [Mount]
        What=rosetta
        Where=/run/rosetta
        Type=virtiofs
        [Install]
        WantedBy=local-fs.target
    - name: rosetta-binfmt.service
      enabled: true
      contents: |
        [Unit]
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
}

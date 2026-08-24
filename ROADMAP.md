# Roadmap

Pending work, grouped by feature. Detailed designs live in
`docs/superpowers/specs/`, execution plans in `docs/superpowers/plans/`.

## macOS (Apple Silicon) backend — branch `rtalur-macos-backend`

Done on hardware: full SNO convergence via the console-driven install→EFI
transition, Rosetta amd64 translation through CRI-O (needed the virtiofs share
mounted with `context=container_file_t` — see the Task 13 notes in the spec),
`easyshift stop`/`start`, and the `--master-disk` flag.

- [ ] **Two-cluster DR check (plan Task 13, step 2–3)**: guest↔guest
      reachability between two NAT clusters on the shared vmnet subnet plus
      host→both-APIs; record the outcome in the spec's "Open risks".
- [ ] **Document the memory envelope**: two concurrently *installing* 16 GB
      SNO VMs thrash a 24 GB host (30 GB swap, apiserver connection refusals).
      Recommend sequential installs (`easyshift stop` the idle cluster while a
      second one installs) or lower `--master-ram`; consider a create-time
      preflight that warns when combined VM RAM exceeds physical RAM.
- [ ] **Bridge mode on macOS**: deferred this phase (`InspectBridge` is a stub
      on darwin). Needs a vmnet bridged-mode story before LAN-reachable
      clusters work on Mac.

## Image baking (`--bake-images`) — branch `rtalur-bake-images`

Complete and green on Linux (libvirt): opt-in flag, multi-arch store baked via
skopeo + virt-make-fs, attached read-only, wired into live-ISO ignition and a
master MachineConfig.

- [ ] **macOS integration** (deliberately deferred to the on-hardware phase;
      whichever branch merges second closes this):
      - merge the baked-store wiring into the HTTP-served ignition in
        `publish-pxe-assets` (the macOS equivalent of the live-ISO merge in
        `embed-ignition-iso`);
      - build the store without libguestfs: `mke2fs -d` (brew `e2fsprogs`)
        instead of `virt-make-fs`, raw image instead of qcow2 (vfkit's
        virtio-blk takes raw);
      - a vfkit-side `ImportDisk` equivalent so `create-master-vms` can attach
        the per-cluster store copy.
- [ ] **End-to-end validation on Linux**: a real `--bake-images` install that
      confirms the node pulls platform images from the store, not quay.io.

## Later phases (per project vision)

- [ ] Worker nodes (`addnode`) — CLI surface exists (`--workers` must be 0
      today).
- [ ] Linux distro coverage beyond the current dev targets; keep mac + Linux
      at feature parity where the hypervisor allows.

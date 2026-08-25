# Roadmap

Pending work, grouped by feature. Detailed designs live in
`docs/superpowers/specs/`, execution plans in `docs/superpowers/plans/`.

## macOS (Apple Silicon) backend — branch `rtalur-macos-backend`

Done on hardware: full SNO convergence via the console-driven install→EFI
transition, Rosetta amd64 translation through CRI-O (needed the virtiofs share
mounted with `context=container_file_t` — see the Task 13 notes in the spec),
`easyshift stop`/`start`, and the `--master-disk` flag.

- [ ] **Two-cluster DR check (plan Task 13, step 2) — deferred, needs a
      bigger host (≥ 48 GB RAM)**: the 24 GB dev Mac cannot keep two active
      16 GB SNO VMs alive (30 GB swap exhausted, apiserver refusals, and the
      second cluster's machine-config daemon wedged its API watches — see the
      2026-08-25 update in the Phase B spike spec). Guest↔guest reachability
      between two NAT clusters on the shared vmnet subnet plus
      host→both-APIs; record the outcome in the spec's "Open risks".
- [ ] **Memory preflight**: warn (or block) at create when the combined RAM
      of running easyshift VMs plus the new master would exceed physical RAM;
      suggest `easyshift stop <other>` or a lower `--master-ram`.
- [ ] **Verify Rosetta from first boot**: dr1 validated the MachineConfig via
      a post-install `oc apply`; a fresh create with the current binary ships
      it in the install ignition — confirm binfmt is live on first boot during
      the next single-cluster validation run.
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

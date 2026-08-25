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

## Image baking (`--bake-images`) — branch `rtalur-bake-images-macos`

Code-complete on both backends (see `docs/dev/image-baking.md`). Linux:
skopeo + virt-make-fs → qcow2 into the libvirt pool. macOS: skopeo +
`mke2fs -d` → raw image, APFS-cloned per cluster, merged into the HTTP-served
ignition. `make check` green; darwin pipeline validated under `--simulate` and
the mke2fs pack validated against real e2fsprogs.

Branch note: `rtalur-bake-images` is the same feature rebased onto `main`
(without the macOS backend); `rtalur-bake-images-macos` stacks it on
`rtalur-macos-backend` and adds the macOS integration. Whichever merge order
is chosen, keep only one of the two bake branches.

- [ ] **End-to-end validation on a real cluster** (either OS): a
      `--bake-images` install confirming CRI-O serves release images from the
      store, not quay.io, in both bootstrap and post-pivot phases. Needs
      ~40+ GB free disk for the multi-arch store — did not fit the dev Mac
      alongside the existing cluster.

## Later phases (per project vision)

- [ ] Worker nodes (`addnode`) — CLI surface exists (`--workers` must be 0
      today).
- [ ] Linux distro coverage beyond the current dev targets; keep mac + Linux
      at feature parity where the hypervisor allows.

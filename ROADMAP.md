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

## Image baking (`--bake-images`) — merged (#17) + follow-up fixes

**Validated end-to-end on macOS hardware (2026-08-25)**: a clean
`create --bake-images` converged in ~25.5 min (storeless baseline: 29 min)
with the store mounted read-only from first boot, CRI-O serving ~173 store
images via the crio drop-in, only 8 network image pulls and 1.0 GiB total RX
in 20 min (the broken run pulled 7.1 GiB in five). Rosetta amd64 execution
also verified from first boot. Three hardware-found fixes live on branch
`rtalur-bake-followups`: APFS-clone-aware disk preflight, `create manifests`
before extra-manifest drops (they were silently ignored), and the CRI-O
drop-in replacing the unsupported `storage.conf.d` mechanism.

- [ ] **Productize the macOS store builder.** skopeo cannot author an overlay
      container store on macOS (Linux-only graph driver), so the baker's
      skopeo step fails on a Mac. The validated workaround built the store
      inside the podman machine and copied out `store.img` (bake stage's
      `Ready()` then skips the build). Implement that as the darwin baker
      backend (podman machine or any Linux builder), including multi-arch
      (`--all`) copies — the spike store was aarch64-only.
- [ ] **Linux-side validation**: the qcow2/virt-make-fs/libvirt-pool variant
      of the attach path has not run against a real Linux host.
- [ ] **Offline installs** (the end-goal baking enables). Residual online
      dependencies measured on hardware (2026-08-25): the three OLM catalog
      indexes (fix: ship an OperatorHub disableAllDefaultSources manifest when
      baking), the two insights-runtime images whose imagePullPolicy=Always
      bypasses the store (fix: disable Insights, disconnected-style), and —
      the real blocker — magic DNS: sslip.io names resolve via public
      nameservers, so api/api-int/*.apps are unresolvable offline on both host
      and node. Needs a local answer for the cluster domain (host dnsmasq /
      hosts injection + node-side resolution) or a non-magic local domain
      mode. Binaries + RHCOS are already cached per version.

## Later phases (per project vision)

- [ ] Worker nodes (`addnode`) — CLI surface exists (`--workers` must be 0
      today).
- [ ] Linux distro coverage beyond the current dev targets; keep mac + Linux
      at feature parity where the hypervisor allows.

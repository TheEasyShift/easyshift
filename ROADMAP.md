# Roadmap

Pending work now lives in [GitHub issues](https://github.com/TheEasyShift/easyshift/issues);
this file is the index. Detailed designs live in `docs/superpowers/specs/`,
execution plans in `docs/superpowers/plans/`.

## macOS (Apple Silicon) backend — merged

Done on hardware: full SNO convergence via the console-driven install→EFI
transition, Rosetta amd64 translation through CRI-O from first boot,
`easyshift stop`/`start`, `--master-disk`, and the host memory preflight.

- [#23](https://github.com/TheEasyShift/easyshift/issues/23) — two-cluster
  DR check (needs a ≥ 48 GB host)
- [#24](https://github.com/TheEasyShift/easyshift/issues/24) — bridge mode
  on macOS
- [#30](https://github.com/TheEasyShift/easyshift/issues/30) — vmnet-helper
  sidecar dies with the invoking process group

## Image baking (`--bake-images`) — merged, macOS-validated

End-to-end validated on macOS hardware (2026-08-25): store served ~173
images, 8 network pulls / 1.0 GiB RX total, Rosetta from first boot.

- [#25](https://github.com/TheEasyShift/easyshift/issues/25) — productize
  the macOS store builder (podman machine)
- [#26](https://github.com/TheEasyShift/easyshift/issues/26) — Linux-side
  validation of the qcow2/libvirt path

Offline installs are **not planned**: magic DNS resolves through public
nameservers by design, and easyshift accepts that online dependency. The
2026-08-25 gap analysis (catalog indexes, `imagePullPolicy: Always` images,
DNS) is preserved in git history should this ever revisit.

## Storage (`--odf`) — merged (trim, never disable)

See [docs/dev/odf.md](docs/dev/odf.md) and the
[design spec](docs/superpowers/specs/2026-08-25-odf-single-node-design.md).

- [#27](https://github.com/TheEasyShift/easyshift/issues/27) — validate
  full-feature `--odf` on a ≥ 36 GB host
- [#28](https://github.com/TheEasyShift/easyshift/issues/28) —
  `--odf-profile` (ODF native resourceProfile + count knobs)
- [#29](https://github.com/TheEasyShift/easyshift/issues/29) — day-2
  `easyshift odf remove`

## Later phases (per project vision)

- [#31](https://github.com/TheEasyShift/easyshift/issues/31) — worker nodes
  (`addnode`)
- [#32](https://github.com/TheEasyShift/easyshift/issues/32) — Linux distro
  coverage and mac/Linux feature parity

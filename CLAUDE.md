# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`easyshift` is an opinionated OpenShift installer for developers. It is a Go CLI (cobra) that provisions single-node OpenShift (SNO) clusters as libvirt VMs on the local host. It talks to the `qemu:///system` libvirt instance and shells out to `virsh` / `virt-install`. It does **not** require root — libvirt-group access to `qemu:///system` is sufficient; stages that touch libvirt surface a clear permission error otherwise.

Full documentation lives in `docs/` (see [docs/README.md](docs/README.md)): `docs/user/` for installing/configuring/using, `docs/dev/` for internals. Prefer updating those docs over expanding this file; keep this file a concise orientation map.

## Commands

The Makefile is the source of truth — `go build` directly skips the vet + gofmt checks the Makefile enforces.

- `make` / `make build` — vet, gofmt check, then build the `easyshift` binary at the repo root.
- `make test` — gofmt + vet, then `go test ./...` (unit tests exist and must pass).
- `make check` — lint + build + test. Run before pushing.
- `make lint.go/make lint.go.full` — lint the go code: vet + fmt + `golangci-lint`.
- `make lint.make` — runs `checkmake` against the Makefile (`maxBodyLength = 8` in `checkmake.ini`).
-  make lint - runs all linters.
- `make fix.go.fmt` — apply `go fmt` to fix formatting.

`golangci-lint` (v2.12.2) and `checkmake` (v0.3.2) are pinned and invoked via `go run ...@version` — do not assume a system install. golangci-lint is configured by `.golangci.yml` (errcheck excludes the `fmt.Fprint*` family).

## Architecture

Six top-level packages with a strict acyclic dependency flow (details in [docs/dev/architecture.md](docs/dev/architecture.md)):

```
config  ←  interfaces  ←  ┬─ stages/*      ┐
                          └─ providers/*   ┘  ←  app  ←  cmd
```

- `config/` — plain data + constants + path helpers. No behavior. `LoadConfig(dir)` returns a `*Config` (no singleton, no global state, no mutex); persisted as JSON at `~/.config/easyshift/config.json` (mode `0600`). Holds the cluster list and `GlobalState` (used IPs/MACs, allocated across all clusters).
- `interfaces/` — every behavioral interface plus the spec/DTO types they exchange (`Deps`, `StageContext`, `Stage`, `NetworkSpec`, …). Depends only on `config`.
- `stages/<one package per step>` — each install step. A stage holds the interfaces it needs as struct fields, injected by `New(...)`. Imports only `config` + `interfaces` — never `providers`, never another stage.
- `providers/<x>` — concrete implementations (`exec`, `libvirt`, `openshift`, `fileserver`, `dns`, `tls`, `host`, `csr`, plus `fakes`). Each is self-contained: never imports another provider or a stage.
- `app/` — the assembler. Builds `Deps`, owns the lifecycle `ClusterManager` and the stage `Runner`. **The only package that imports concrete providers and stage packages.**
- `cmd/easyshift/` — cobra CLI; flag parsing; chooses production vs `--simulate` wiring.

### Staged installer

A cluster install is an ordered list of `Stage`s (`app/manager.go:buildStages`) driven by the `Runner`, with progress persisted in `clusters/<name>/state.json`. `create` is idempotent and resumes from the first unfinished stage; `delete` rolls applied stages back in reverse then removes the cluster dir. Each stage's `Apply` must tolerate retry; `Rollback` must tolerate partial state. Optional `Preflight` checks run on every stage and are aggregated before any `Apply`. See [docs/dev/stages.md](docs/dev/stages.md).

Dependency injection: stages get exactly the interfaces they need (not the whole `Deps`); `Deps` (`interfaces/deps.go`) is the wiring bag populated by `app/deps.go:NewProductionDeps` (production) or `providers/fakes` (tests + `--simulate`).

### Key constraints (validated in `app/manager.go:validateNew`)

- SNO only: `MasterCount` **must** be 1, `WorkerCount` **must** be 0 (current phase).
- Total clusters ≤ `DefaultClustersMax` (3).
- `--magic-dns` is mutually exclusive with both `--dns-provider` and `--base-domain`.
- Bridge mode requires `--bridge` + `--master-mac` + `--master-ip`; NAT mode rejects them (MAC/IP/CIDR are auto-assigned).

### Networking

- **NAT mode** (default): all NAT clusters share ONE global libvirt network `easyshift-nat` (subnet `192.168.126.0/24`) so clusters share an L2 segment and can reach each other (DR topologies). Per-cluster identity is a DHCP reservation, not a separate network; deleting one cluster removes only its reservation. Magic DNS (`sslip.io`/`nip.io`) makes names resolve to the master IP with no DNS server.
- **Bridge mode**: VMs attach to an existing host Linux bridge for LAN reachability; needs real DNS (manual or `--dns-provider cloudflare`) and optionally Let's Encrypt TLS (`--tls-email`, ACME DNS-01).
- VMs are named `<role>-<index>-<cluster>` (e.g. `master-0-mycluster`). The ignition file server runs on port `9393`.

### External tooling assumed on PATH

`virsh`, `virt-install`, `ssh-keygen`, `tar`, `dig`. The OpenShift binaries (`openshift-install`, `oc`, `coreos-installer`) are downloaded per cluster from `mirror.openshift.com` — not user-installed.

### `--simulate`

`easyshift <cmd> --simulate` runs the full pipeline against in-memory fakes in a throwaway config dir and prints a trace of every operation. Backs both a user dry-run and the unit tests. See [docs/dev/testing.md](docs/dev/testing.md).

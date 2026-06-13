# macOS (Apple Silicon) support with Rosetta — design

Date: 2026-06-13
Status: Approved design, pending implementation plan

## Goal

Let `easyshift` provision single-node OpenShift (SNO) clusters on macOS running
on Apple Silicon, with the same opinionated, no-root, resumable, multi-cluster
experience it gives on Linux today. The guest is **native arm64** RHCOS/OpenShift,
and Apple's **Rosetta** is exposed inside the guest so x86-64 container workloads
run translated.

This is the macOS half of the standing project vision (mac + linux, NAT or
bridge, single master). It does not change the SNO/single-master constraints.

### Non-goals (this phase)

- Running x86-64 OpenShift under emulation. Rosetta is a userspace translation
  layer (binfmt_misc for x86-64 ELF inside an arm64 Linux guest); it cannot boot
  an x86-64 kernel, and full QEMU TCG system emulation is too slow for a cluster.
  The guest is always arm64.
- macOS **bridge mode** (LAN reachability) and the real-DNS / Let's Encrypt path.
  Deferred; NAT + magic DNS only.
- Intel Macs. Apple Silicon only.

## Decisions (already settled)

1. **Rosetta scope:** native arm64 guest + Rosetta for x86-64 *workloads*.
2. **Compute backend:** shell out to **vfkit** (signed Go CLI from the CRC/podman
   team that drives Apple's Virtualization.framework). No cgo, no Swift, no
   entitlement on the `easyshift` binary — vfkit ships pre-signed with the
   virtualization + Rosetta entitlements.
3. **Networking backend:** **vmnet-helper** in shared mode, with vfkit attaching
   to it over a socket fd (`--device virtio-net,fd=…`, a
   `VZFileHandleNetworkDeviceAttachment`). gvproxy was considered and dropped (see
   "Networking" below).
4. **v1 scope:** NAT + **multi-cluster DR parity** (multiple arm64 clusters on one
   shared L2 segment that can reach each other), magic DNS, Rosetta. Bridge mode
   and real DNS deferred.

## Why vmnet-helper (and not gvproxy or raw vmnet)

vfkit on its own only does opaque `VZNATNetworkDeviceAttachment` (no subnet
control). It documents two ways to get a controllable network:

- **gvisor-tap-vsock (gvproxy)** — userspace TCP/IP stack, full addressing
  control, no privilege, but the host is *not* on the guest segment, so reaching
  N clusters' API servers from the mac means per-cluster port-forwards (6443
  collides across clusters) or a host-route workaround. Awkward for multi-cluster.
- **vmnet-helper** — holds the privilege/entitlement and bridges a real macOS
  vmnet network to vfkit via an fd. Apple's current `vmnet.framework` exposes a
  typed network-configuration API
  (`vmnet_network_configuration_set_ipv4_subnet`,
  `vmnet_network_configuration_add_dhcp_reservation`,
  `…_add_port_forwarding_rule`, `vmnet_interface_start_with_network`), and
  vmnet-helper surfaces it as CLI flags: `--operation-mode=shared`,
  `--subnet-mask`, `--start-address` (gateway), `--end-address` (DHCP pool end),
  `--interface-id`. Shared-mode interfaces communicate with each other.

vmnet-helper gives us **both** things that previously split the decision:
deterministic, controllable per-cluster addressing **and** native host-on-subnet
reachability (the mac is on the vmnet subnet, so `oc` reaches every cluster's API
directly, exactly like libvirt's `virbr0` gateway) — without an entitlement on
`easyshift`. That decisively favors it for multi-cluster DR.

### Mapping to the libvirt model

| libvirt (Linux) | macOS equivalent |
|---|---|
| `libvirtd` (privileged daemon; user is in `libvirt` group) | `vmnet-helper` (privileged helper; `sudo` or launchd install) |
| `<network>` with chosen subnet | `vmnet-helper --subnet-mask/--start-address/--end-address` |
| one shared `easyshift-nat`, many VMs → DR | one shared-mode vmnet network, many VMs → DR |
| `<host mac= ip=>` DHCP reservation | vmnet DHCP reservation, or pin via the existing ignition keyfile |
| host on `virbr0` gateway → `oc` reaches every cluster | host on vmnet subnet (start-address = gateway) → same |
| `virsh` / `virt-install` shelled out | `vfkit` + `vmnet-helper` shelled out |

## Architecture

Single universal binary, no cgo. Production wiring branches on `runtime.GOOS` in
`app/deps.go`:

- Linux → existing `NewProductionDeps` (libvirt providers).
- macOS → new `NewDarwinDeps` returning a `Deps` bag with the vfkit + vmnet-helper
  providers.

The `interfaces` package is unchanged in shape; only new concrete providers and a
second wiring path are added. The stage list (`app/manager.go:buildStages`) stays
identical except for a per-OS boot-media stage and a rename (below).

### New / changed packages

- `providers/vfkit` — implements `interfaces.VMManager`. **Process supervisor**,
  not a daemon client: unlike libvirtd (which owns persistent domains), vfkit is
  one foreground process per running VM. So:
  - `Create` writes a launch spec (vfkit args, disk path) into the cluster dir.
  - `Start` spawns vfkit detached, records its pid + its `--restful-uri` control
    socket.
  - `IsRunning` / `Stop` use the vfkit REST API (and pid as a fallback).
  - `Delete` tears down the spec + disk image.
  - The existing `waitforinstall` watchdog ("restart a shut-off VM") maps onto
    `IsRunning` + `Start`.
  - `ImportISO` / storage-pool concepts are libvirt-only; on mac they are no-ops
    or unused because boot uses network/PXE assets (see "Boot & ignition").
- `providers/vmnethelper` — implements `interfaces.NetworkProvisioner`. Owns the
  single shared vmnet network used by all NAT clusters:
  - `EnsureNetwork` → ensure a shared-mode vmnet-helper network exists with our
    chosen subnet (candidate: reuse `192.168.126.0/24` to mirror Linux). Idempotent.
  - `AddHost` / `RemoveHost` → per-cluster identity: a vmnet DHCP reservation
    where exposed, otherwise tracked in `GlobalState` and pinned via the ignition
    static keyfile. Deleting one cluster removes only its reservation.
  - `InspectNetwork` → report subnet / reservations / leases for `status`.
  - Provides the socket fd that the vfkit VMManager attaches to.
- `providers/host` — split the OS-specific checks into build-tagged files
  (`host_linux.go` / `host_darwin.go`). The darwin `HostInspector`:
  - `HasCPUVirtualization` → confirm Apple Silicon + macOS version supports
    Virtualization.framework + Rosetta-for-Linux (macOS 13+).
  - `InspectBridge` / `ARPLookup` → darwin equivalents (or unused while bridge
    mode is deferred). `/proc/cpuinfo`, `/sys/class/net`, `/proc/net/arp` have no
    macOS equivalent and must not be referenced on darwin.
  - `LookPath` → must find `vfkit` and `vmnet-helper`.

### Architecture parameterization

`providers/openshift` currently hardcodes `coreOSArch = "x86_64"` and the
`-linux` client tarball suffix. Parameterize by target:

- Guest RHCOS artifact: `aarch64`.
- Host tools (run on the mac to generate ignition / drive install): the
  `mac-arm64` builds from the mirror — `openshift-install-mac-arm64.tar.gz`,
  `openshift-client-mac-arm64.tar.gz`.

## Boot & ignition flow on macOS

Instead of the Linux path (coreos-installer embeds ignition into a live ISO,
uploaded to a libvirt storage pool), macOS uses **network / PXE-style boot**,
reusing machinery that already exists:

- vfkit's Linux bootloader (`--bootloader linux,kernel=…,initrd=…,cmdline=…`)
  boots the RHCOS **aarch64** live kernel + initramfs + rootfs.
- The kernel cmdline carries `ignition.config.url=http://<host>:9393/…` plus the
  rootfs URL, served by the **ignition fileserver easyshift already runs on
  :9393**.

This avoids needing a macOS build of `coreos-installer` on the host (availability
is unreliable), drops the libvirt storage-pool / `ImportISO` dependency on mac,
and leans on the existing `VMSpec.KernelArgs` field. The Linux `embed-ignition-iso`
stage is replaced on mac by a `publish-pxe-assets` stage (per-OS selection in
`buildStages`); the other stages are unchanged.

## Rosetta enablement

Two halves:

- **Host:** launch vfkit with `--device rosetta,mountTag=rosetta` (exposes the
  Rosetta translator as a virtiofs share).
- **Guest:** an ignition / MachineConfig fragment mounts the `rosetta` virtiofs
  tag and registers a `binfmt_misc` handler for x86-64 ELF, so `amd64` container
  images run translated. This is a small new guest-config artifact added to the
  ignition we already generate.

## Stage & wiring changes (summary)

- Rename `stages/createlibvirtnetwork` → `stages/createnetwork`. It only calls
  `Net.EnsureNetwork` / `Net.AddHost`, so it is already backend-agnostic — just
  misnamed.
- Add `stages/publishpxeassets` (mac) selected in place of `embedignitioniso`
  (linux) by `buildStages`.
- `app/status.go:checkVMState` currently shells out to `virsh domstate`; route VM
  state through `VMManager.IsRunning` so `status` works on both backends.
- `app/deps.go`: add `NewDarwinDeps`; branch on `runtime.GOOS`.
- Parameterize architecture in `providers/openshift` (`coreOSArch`, client tarball
  suffix).

## What carries over unchanged

The config / allocation layer is backend-agnostic and moves as-is: `GlobalState`
IP/MAC allocation, the reservation model, per-cluster magic-DNS names, the
`DefaultClustersMax` (3) cap, the SNO / single-master constraints, and the
"delete removes only this cluster's identity" semantics. Magic DNS
(sslip.io / nip.io) keyed to each master IP works because the host is on the
vmnet subnet and the IPs are reachable.

## Preflight on macOS

- Apple Silicon + macOS ≥ 13 (Rosetta-for-Linux requirement).
- `vfkit` and `vmnet-helper` present (on PATH, or downloaded/pinned per-version
  like the OpenShift binaries — to be decided).
- **vmnet-helper privilege:** vmnet-helper needs elevation to open the vmnet
  interface — run via `sudo` or installed once as a launchd daemon. This is the
  macOS analog of "be in the libvirt group" and must be a clear preflight check
  with an actionable error, plus user docs. It is a *helper* requirement, not an
  `easyshift`-binary signing requirement.
- Disk-space and TCP-dial checks are already platform-agnostic.

## Phasing

Even though v1 targets multi-cluster DR parity, sequence it:

1. One arm64 SNO booting under vfkit with network ignition (no DR yet).
2. vmnet-helper shared networking + magic DNS + deterministic per-cluster IP.
3. Rosetta in the guest.
4. A second cluster on the shared network to prove DR (guest↔guest reachability
   and host reaching both APIs).

Deferred: macOS bridge mode (needs extra entitlements / privilege), real DNS +
Let's Encrypt.

## Testing

The existing `fakes` + `--simulate` harness abstracts all of this behind the
interfaces, so a darwin fake wiring lets the full pipeline run in `--simulate` on
any OS (including Linux CI). Real vfkit / vmnet-helper execution stays in
manual / e2e territory, same as `virsh` today.

## Open risks to validate early

- vmnet-helper multiplexing multiple vfkit VMs on one shared network for the DR
  case. The vfkit + helper combo is proven for single VMs (CRC, lima, podman);
  N peers on one shared segment is the least-exercised path. The two-cluster
  spike in phase 4 is the gate.
- RHCOS aarch64 live PXE assets booting under vfkit's Linux bootloader with our
  cmdline (vs. the ISO path).
- Whether vmnet-helper exposes per-MAC DHCP reservations directly, or we rely on
  the ignition static-keyfile pin within our chosen subnet.
- Bundling vs. requiring `vfkit` / `vmnet-helper` on PATH, and the one-time
  vmnet-helper privilege install UX.

## References

- Apple `vmnet` framework: https://developer.apple.com/documentation/vmnet
- vfkit usage (networking): https://github.com/crc-org/vfkit/blob/main/doc/usage.md
- vmnet-helper: https://github.com/nirs/vmnet-helper (integration.md for the
  subnet / shared-mode flags)

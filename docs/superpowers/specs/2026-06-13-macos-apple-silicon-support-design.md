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

### Development environment (validated 2026-06-13)

The primary dev machine is the target hardware: **macOS 26.5.1 (Tahoe), Apple
Silicon (arm64)**, with `vfkit 0.6.3` (on `PATH`) and `vmnet-helper 0.12.0`
(Homebrew, under `libexec`) installed. So on-hardware validation (real vfkit
boot, vmnet-helper shared network, Rosetta, two-cluster DR) is **in scope** and
executed on this machine, not deferred to separate hardware. CI remains Linux
(`ubuntu-latest`), where the macOS code paths are exercised via `--simulate`
and cross-compilation only.

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
3. **Networking backend:** **vmnet-helper** in shared mode, run as a **per-VM
   sidecar** that vfkit attaches to over a socket fd (`--device
   virtio-net,fd=…`, a `VZFileHandleNetworkDeviceAttachment`). vmnet-helper's
   CLI *requires* `--fd FD | --socket SOCKET`, so it is paired with exactly one
   VM rather than being a standalone network daemon (see "Networking" and "VM
   lifecycle coupling" below). gvproxy was considered and dropped.
4. **v1 scope:** NAT + **multi-cluster DR parity** (multiple arm64 clusters on one
   shared L2 segment that can reach each other), magic DNS, Rosetta. Bridge mode
   and real DNS deferred.
5. **Shared subnet:** reuse `192.168.126.0/24` (same as the Linux `easyshift-nat`
   network) so the two platforms behave identically. This requires the **per-VM
   shared-mode** path (`--operation-mode shared --start-address 192.168.126.1
   --subnet-mask 255.255.255.0`, same subnet for every cluster's sidecar → shared
   L2 → DR). The macOS-26 `vmnet-broker` `--network NAME` mode is cleaner for the
   shared network but is mutually exclusive with `--start-address/--subnet-mask`
   (the broker owns the subnet, defaulting to `192.168.64.0/24`), so we do **not**
   use broker mode in v1; the spike confirms shared-subnet DR.
6. **Per-cluster IP determinism:** pin each master's IP via the **existing
   ignition static NetworkManager keyfile** (allocated from `GlobalState`),
   within the shared subnet. This already exists and behaves identically on both
   OSes, so we do **not** depend on vmnet-helper exposing per-MAC DHCP
   reservations.
7. **vfkit / vmnet-helper distribution:** documented prerequisites (like `virsh` /
   `virt-install` on Linux), not bundled or auto-downloaded. `vfkit` is a normal
   `PATH` binary. **vmnet-helper is NOT on `PATH`** — Homebrew installs it under
   `$(brew --prefix vmnet-helper)/libexec/vmnet-helper` (upstream `install.sh`
   uses `/opt/vmnet-helper/bin/vmnet-helper`). Preflight resolves the binary at
   those known locations rather than via a bare `LookPath("vmnet-helper")`.
8. **vmnet-helper privilege:** a one-time, **user-performed** setup — install the
   shipped sudoers rule (`sudo install -m 0640
   $(brew --prefix vmnet-helper)/share/doc/vmnet-helper/sudoers.d/vmnet-helper
   /etc/sudoers.d/`), the macOS analog of joining the `libvirt` group. The rule
   grants `%staff ALL=(root) NOPASSWD: <vmnet-helper path>` plus
   `closefrom_override`, and the helper is invoked as `sudo --non-interactive
   --close-from N <vmnet-helper> --fd M …` (the `--close-from` is required to pass
   the socket fd through sudo). **Caveat:** the shipped rule hardcodes
   `/opt/vmnet-helper/bin/vmnet-helper`, which does not match the Homebrew
   libexec path; preflight must verify the NOPASSWD path matches the binary
   easyshift actually invokes, and docs must tell brew users to adjust the rule
   (or symlink) accordingly.

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
  - **Sidecar ownership (see "VM lifecycle coupling"):** because the network is a
    per-VM vmnet-helper process, `Start` also spawns that sidecar (via sudo) and
    wires its socket into vfkit's `virtio-net,fd=…`; `Stop`/`Delete` reap it.
- `providers/vmnethelper` — implements `interfaces.NetworkProvisioner`, plus a
  sidecar-launch seam the vfkit VMManager calls at `Start`. It does **not** own a
  standalone network daemon (vmnet-helper requires `--fd/--socket` and is bound to
  one VM):
  - `EnsureNetwork` → validate/record that the shared network identity
    (`192.168.126.0/24`, shared mode, decision 5) is the one all per-VM sidecars
    will use. There is no separate "create the network" call to make idempotent;
    the network materializes when the first sidecar starts. Effectively a
    config/validation step on macOS.
  - `AddHost` / `RemoveHost` → per-cluster identity: the IP/MAC is allocated from
    `GlobalState` and pinned via the ignition static keyfile (decision 6); these
    methods record/clear that allocation. Deleting one cluster removes only its
    entry.
  - `InspectNetwork` → report what the provider knows for `status`.
  - `StartSidecar(vmName, mac) (socketPath, cleanup, error)` → resolve the
    vmnet-helper binary (decision 7), spawn it under `sudo --non-interactive
    --close-from N … --operation-mode shared --start-address 192.168.126.1
    --subnet-mask 255.255.255.0 --socket <path>` (decision 8), and hand the socket
    path back to the vfkit VMManager. This is the seam that couples the two
    providers; it lives behind a small extra interface, not the stage-facing
    `NetworkProvisioner`.

### VM lifecycle coupling (macOS-specific)

On Linux the network (libvirt) and VM (libvirt) lifecycles are independent: the
NAT network is a host-global resource and `virt-install` just attaches to it. On
macOS the vmnet-helper process is **per VM and privileged**, so VM start and
network attach are one operation: the vfkit VMManager, at `Start`, asks the
vmnethelper provider for a sidecar socket, then launches vfkit attached to it;
at `Stop`/`Delete` it tears the sidecar down. This coupling is contained inside
`providers/vfkit` + `providers/vmnethelper`; stages and the rest of the app are
unaffected (they still see plain `VMManager` + `NetworkProvisioner`).
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
- `vfkit` present on `PATH`; **vmnet-helper resolved at its known install
  locations** (`$(brew --prefix vmnet-helper)/libexec/vmnet-helper` or
  `/opt/vmnet-helper/bin/vmnet-helper`), not via bare `LookPath` (decision 7).
- **vmnet-helper privilege:** verify the sudoers rule is installed and its
  `NOPASSWD` path matches the binary easyshift will invoke — concretely, that
  `sudo --non-interactive <resolved-vmnet-helper> --version` succeeds without a
  prompt. On failure, the preflight error must name the exact install command
  (decision 8) and the path-mismatch caveat. This is a *helper* requirement, not
  an `easyshift`-binary signing requirement.
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

Two layers. (1) **Unit / simulate (Linux CI + local):** the `fakes` +
`--simulate` harness abstracts everything behind the interfaces, so the darwin
wiring runs the full pipeline in `--simulate` and provider command/arg
construction is unit-tested against the fake `CommandRunner` (as the libvirt
provider already is). (2) **On-hardware (this Apple Silicon machine):** because
the dev box is the target platform with vfkit + vmnet-helper installed, the
real-boot path is validated here — boot an aarch64 SNO, stand up the shared
vmnet network, confirm Rosetta in-guest, and run the two-cluster DR check. This
is no longer deferred to separate hardware.

## Open risks to validate early (on this machine)

- vmnet-helper running one sidecar per vfkit VM, all on the same shared subnet,
  giving guest↔guest reachability across clusters (DR) and host reachability to
  every cluster API. Proven for single VMs (CRC, lima, podman); the N-peer
  shared-subnet case is the least-exercised path — the two-cluster spike is the
  gate.
- RHCOS aarch64 live PXE assets booting under vfkit's Linux bootloader with our
  cmdline (vs. the ISO path).
- The sudoers path-mismatch (brew `libexec` vs the rule's `/opt/vmnet-helper/bin`)
  and the `--close-from` fd-passing handshake working end to end.

## References

- Apple `vmnet` framework: https://developer.apple.com/documentation/vmnet
- vfkit usage (networking): https://github.com/crc-org/vfkit/blob/main/doc/usage.md
- vmnet-helper: https://github.com/nirs/vmnet-helper (integration.md for the
  subnet / shared-mode flags)

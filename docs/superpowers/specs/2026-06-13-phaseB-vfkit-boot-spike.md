# Phase B vfkit boot spike — findings & locked architecture

Date: 2026-06-13
Hardware: Mac mini, Apple Silicon (M-series), macOS 26.5.1, 24 GB RAM
Tools: vfkit 0.6.3, vmnet-helper 0.12.0, openshift-install 4.22.0 (mac-arm64),
RHCOS 9.8 aarch64 live ISO.

## Why this spike

The Phase A plan assumed a PXE `--kernel`/`--initrd` boot. Mapping the real
pipeline surfaced that bootstrap-in-place needs a **post-install reboot into the
installed disk**, which a fixed kernel/initrd boot cannot do (it reloads the
live kernel forever). So the boot method was an open risk; we spiked it on real
hardware before implementing.

## What was tested (manual vfkit invocations)

1. **EFI boot of the RHCOS aarch64 live ISO** —
   `vfkit --bootloader efi,variable-store=…,create --device usb-mass-storage,path=<iso>,readonly --device virtio-blk,path=<disk> --device virtio-net,nat --device virtio-serial,logFilePath=<log>`.
2. **vfkit `--ignition <file>`** with a marker systemd unit, to see if RHCOS
   applies it.
3. **`coreos-installer` availability on macOS** (Homebrew + PATH).

## Results

| Question | Result |
|---|---|
| vfkit EFI-boots the RHCOS aarch64 live ISO | ✅ reached the CoreOS live shell |
| Headless observability | ✅ `virtio-serial,logFilePath` captures the full guest console (banner + prompt). **No GUI needed.** |
| `usb-mass-storage` (ISO) + `virtio-blk` (disk) + `rosetta` devices attach | ✅ all accepted |
| REST API (`--restful-uri tcp://…`) reports VM state | ✅ `/vm/state` works; `--pidfile` works |
| vfkit `--ignition` honored by RHCOS live | ❌ ignored (RHCOS `metal` platform; vfkit's ignition targets FCOS-style vsock fetch) |
| `coreos-installer` on macOS | ❌ no Homebrew formula (`coreos-ct` only), no official mac build |

### Consequence

Both standard ignition-delivery paths are unavailable on macOS:
- **ISO embed** — needs `coreos-installer` (not on mac).
- **vfkit `--ignition`** — ignored by RHCOS metal.

The only remaining delivery is **network ignition** (`ignition.config.url=…`
over the existing `:9393` fileserver), which requires controlling the kernel
cmdline → `--bootloader linux` (live kernel/initrd). But that bootloader can't
boot the installed disk after the reboot. EFI can.

## Locked architecture: two-phase vfkit supervisor

The vfkit `VMManager` drives two phases per VM, switching bootloaders:

- **Install phase** — `--bootloader linux` with the RHCOS live **kernel +
  initramfs** (PXE assets), cmdline we control:
  `coreos.live.rootfs_url=http://<host>:9393/<cluster>/rootfs.img ignition.config.url=http://<host>:9393/<cluster>/config.ign ignition.firstboot ignition.platform.id=metal console=hvc0 …`.
  Devices: `virtio-blk` (install target disk), the vmnet-helper socket NIC,
  `virtio-serial` log, `rosetta`. The bootstrap-in-place ignition installs RHCOS
  to the disk.
- **Run phase** — when the install phase ends (guest powers off / VM stops), the
  supervisor relaunches with `--bootloader efi,variable-store=…,create` + the
  **disk only** (no kernel/initrd, no ISO). EFI boots the installed system.

The supervisor persists which phase a VM is in (state file) so it survives
across `easyshift` invocations and the watchdog relaunches the correct phase.

### Notes / caveats to handle in implementation

- **arm64 kernel must be uncompressed** for `--bootloader linux` (vfkit errors on
  a compressed kernel). RHCOS `live-kernel.aarch64` may be gzip'd → gunzip before
  use.
- **Serial console**: pass `console=hvc0` in the install cmdline so logs land in
  the `virtio-serial` file (the live ISO's own GRUB already maps to the vfkit
  console, but the linux-bootloader path sets the cmdline ourselves).
- **Phase transition detection**: the install phase ends when the guest stops.
  vfkit exits / the VM goes to `VirtualMachineStateStopped`; the supervisor
  detects this (pidfile gone / REST state) and relaunches in EFI mode.
- **Still to validate (one long ~30–45 min test)**: that a real SNO
  bootstrap-in-place install under the linux bootloader completes, the guest
  stops, and the EFI relaunch boots the installed disk to a Ready node.

## Revised Task 11 scope (supersedes the plan's PXE-only Task 11)

1. **Installer**: add `CoreOSLivePXEURLs` (parse `metal.formats.pxe.{kernel,
   initramfs,rootfs}` from `print-stream-json`; the spike confirmed all three
   exist for aarch64). Add to `interfaces.Installer` + fake.
2. **downloadrhcos** (darwin): fetch the 3 PXE assets (kernel/initramfs/rootfs)
   into the RHCOS cache; gunzip the kernel if compressed.
3. **publishpxeassets.Apply**: copy `rootfs.img` + the SNO ignition
   (`bootstrap-in-place-for-live-iso.ign` → `config.ign`) into
   `FileServer.RootDir()/<cluster>/`; record the install-phase kernel/initrd
   local paths + cmdline for the VMManager.
4. **vfkit VMManager**: two-phase Start (linux→efi), detached spawn via
   `--pidfile`, `--restful-uri` for state, phase state file; obtain the
   vmnet-helper socket from the sidecar launcher.
5. **vmnethelper.StartSidecar**: `sudo --non-interactive <bin> --socket <path>
   --operation-mode shared --start-address 192.168.126.1 …`; return socket +
   stop func. Pair with vfkit `--device virtio-net,unixSocketPath=<socket>`.
6. **deps**: pass the vmnethelper sidecar launcher into `vfkit.NewVMManager`
   (new `interfaces.SidecarLauncher`).
7. **Rosetta**: deferred to Task 13 (not needed to boot).
8. **coreos-installer** is a Linux-only host tool; on macOS `downloadbinaries`
   must skip it, and `embed-ignition-iso` is not used (replaced by
   publish-pxe-assets).

## Update 2026-06-14: real-boot result + the post-install reboot problem

A full `easyshift create` on real hardware validated the **entire boot+install
path**: EFI-zboot kernel decode (the live-kernel is a PE32+ `zimg` gzip
payload — must be decompressed to the raw arm64 `Image`), vfkit boots it via
`--bootloader linux`, the vmnet-helper sidecar networks the guest,
bootstrap-in-place installs RHCOS to the disk, and a NetworkManager keyfile
injected into the served ignition pins the node to its allocated IP (verified:
node came up on `.5`, the bootstrap apiserver served there).

**But the cluster never converges** — it reinstall-loops. Root cause, confirmed
from the serial console (the install ignition + ostree-deploy repeats ~12×) and
the vfkit source:

- vfkit blocks on `waitForVMState(Stopped)` and only exits when the VM *stops*.
- On a guest **reboot**, Virtualization.framework restarts the VM **in place**,
  reloading the same `--bootloader linux` kernel — i.e. the *live installer*
  again, not the installed disk. bootstrap-in-place's post-install reboot
  therefore re-runs the installer forever.
- So the two-phase `install→EFI` transition (which keys off vfkit *stopping*)
  never fires: **vfkit does not stop on guest reboot.**
- There is **no vfkit/vz flag** for stop-on-reboot or boot-disk-on-reboot
  (checked `cmd/vfkit/main.go`, the REST API, and `Code-Hex/vz`).

### Two viable fixes (each needs code + more ~40-min boot iterations)

A. **Console-driven transition (smaller change).** A goroutine tails the guest
   serial console for the first post-install reboot (the shutdown sequence after
   the live-env bootstrap completes), then force-stops vfkit and relaunches in
   the EFI run phase to boot the now-installed disk. Works with the current
   network-ignition install. Risk: timing/marker fragility.

B. **EFI + ISO from the start (metal-like, cleanest runtime).** Boot the live
   ISO via `--bootloader efi` + an empty disk (spike-proven to boot). The install
   reboots and EFI firmware boot-order falls through to the now-installed disk —
   exactly how libvirt's `--boot hd,cdrom` works — so there is no reboot loop and
   no transition to manage. Cost: ignition must be embedded in the ISO, and
   macOS has no `coreos-installer`, so this needs a Go reimplementation of
   coreos-installer's ISO ignition-embed (the `coreos-ignition` embed area
   format).

Recommendation: A is the smaller delta on the current (working) install path; B
is architecturally cleaner but needs the embed reimplementation. Either way the
install/boot/network/IP-pin halves are done and committed.

## Reusable spike scratch

`/tmp/vfkit-spike/` holds `openshift-install` (4.22.0 mac-arm64),
`rhcos-live.aarch64.iso`, `stream.json`. The exact working EFI invocation is in
this doc's "What was tested".

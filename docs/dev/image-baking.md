# Baked image store (`--bake-images`)

A fresh single-node OpenShift install pulls the entire release payload — the
release image plus hundreds of component images, several GB — from `quay.io`,
twice: once during the live-ISO **bootstrap** phase and again on the installed
node as the operators roll out **post-pivot**. On a dev box building up to three
clusters that is the dominant chunk of wall-clock and bandwidth.

`--bake-images` pre-pulls that payload into a read-only disk attached to the
master, so CRI-O serves platform images locally and never reaches `quay.io`.
This is the same mechanism Red Hat's `factory-precaching-cli` uses for Telco/ZTP
factory installs, hand-rolled to fit easyshift's stage pipeline and no-root
contract.

## How it works

The store is a CRI-O **additional image store**: a read-only container store
that CRI-O layers underneath its writable one. When the node asks for a release
image, CRI-O finds it locally and skips the pull. Images are stored under their
**original names** (`quay.io/...@sha256:...`), so the digests the release
references match with no `imageDigestMirrors` / ICSP config needed.

### Build (host side) — `stages/bakeimagestore`

Built once per OCP version, cached at
`~/.config/easyshift/imagestore/<version>/`, shared across clusters (rollback is
a no-op, like the binaries cache). `providers/openshift.OpenShiftImageBaker`:

1. `oc adm release info --pullspecs -o json <release-image>` for **every**
   supported arch enumerates the component pullspecs.
2. `skopeo copy --all` copies each into an overlay container store
   (`store/`). `--all` keeps every manifest-list entry.
3. The store is packed into a labeled ext4 disk image (`PackCommand`):
   - **Linux**: `virt-make-fs --type=ext4 --label=baked-images --format=qcow2`
     → `store.qcow2` (rootless via libguestfs; qcow2 because a per-cluster
     copy is uploaded into the libvirt pool).
   - **macOS**: `mke2fs -t ext4 -L baked-images -d <store>` → `store.img`
     (raw — vfkit's virtio-blk takes raw). No libguestfs exists on macOS;
     `mke2fs -d` populates the fs from a directory without mounting. The
     image is sized from the overlay contents (+10% and 1 GiB headroom).
     Homebrew's e2fsprogs is keg-only, so its keg sbin paths are probed
     (`config.MKE2FSCandidates`).

Preflight requires `skopeo` plus the per-OS packer: `virt-make-fs`
(guestfs-tools / libguestfs-tools) on Linux, `mke2fs`
(`brew install e2fsprogs`) on macOS.

### Multi-arch

The store is **multi-arch**. `SupportedReleaseArches` (`x86_64`, `aarch64`) are
each enumerated from their arch-specific release image
(`ocp-release:<version>-<arch>`) and unioned; an arch whose release image
doesn't exist for the version is skipped. One store therefore serves an amd64
node, an aarch64 node, and amd64 workloads run on aarch64 via Rosetta. The
RHCOS live-ISO arch is still selected separately (see
`providers/openshift.coreOSArch`) — baking does not change which ISO boots.

### Attach + wire (node side)

- `stages/createmastervms` attaches a **per-cluster** copy of the store via
  `ImportDisk`: on Linux uploaded into the libvirt pool (read-only +
  shareable; per cluster so `virsh undefine --remove-all-storage` on delete
  never strands another cluster), on macOS APFS-cloned into the vfkit state
  dir (vfkit has no read-only virtio-blk, so the per-cluster copy is the
  isolation; `Delete` removes the clone).
- The disk is mounted by label (`/dev/disk/by-label/baked-images` →
  `/var/lib/baked-images`) and registered with CRI-O via a **CRI-O drop-in**
  (`/etc/crio/crio.conf.d/10-baked-images.conf`,
  `storage_option = ["overlay.imagestore=…"]`). NOT via
  `/etc/containers/storage.conf.d/`: RHCOS 9.8's containers-common (5.8)
  silently ignores that dir — validated on hardware, where the store sat
  mounted-but-unread and every image still came from quay.io until the CRI-O
  drop-in surfaced all of them.
- That wiring is applied in **both** install phases:
  - **post-pivot:** a master `MachineConfig` dropped into the install dir's
    `openshift/` (`Installer.WriteImageStoreManifest`) so it is rendered into
    the node's ignition and present from first boot, before CRI-O pulls
    operators.
  - **bootstrap:** the same file + mount unit merged into the bootstrap
    ignition (`Installer.MergeImageStoreIntoLiveISOIgnition`) — on Linux into
    `bootstrap-in-place-for-live-iso.ign` before the ISO is embedded
    (`embed-ignition-iso`), on macOS into the HTTP-served `config.ign`
    (`publish-pxe-assets`).

Renderers live in `providers/openshift/baker.go` (`RenderCRIODropin`,
`RenderMountUnit`, `RenderMachineConfig`, `MergeBakedStoreIntoIgnition`) and are
unit-tested in `baker_test.go`.

**Extra-manifest ordering (hardware-validated):** `openshift-install create
single-node-ignition-config` only renders manifests dropped into `openshift/`
if `create manifests` ran first — without that state they are silently
ignored. `generate-ignition` therefore calls `Installer.CreateManifests`
before the manifest writes whenever extras are needed (darwin or
`--bake-images`).

**macOS store-build limitation:** skopeo cannot author a
`containers-storage:` overlay store on macOS (the overlay graph driver is
Linux-only), so the coded skopeo path in the baker cannot run on a Mac. The
validated workaround builds the store inside a Linux VM (e.g. the podman
machine: skopeo into an overlay store on the machine's own fs, `mke2fs -d`
pack there, copy out the single `store.img` to
`~/.config/easyshift/imagestore/<version>/`); the bake stage's `Ready()`
probe then skips the build. Productizing that builder is tracked in
ROADMAP.md.

## Verification boundary

The pipeline, the rendered artifacts, and the `--simulate` trace are covered by
unit + app tests. What needs a **real cluster** to confirm:

- CRI-O actually resolves release images from the mounted store (no `quay.io`
  pull) in both phases.
- `create single-node-ignition-config` picks up the MachineConfig dropped in
  `openshift/` (the documented additional-manifest path; verify it is rendered
  into the node ignition).
- The mount unit ordering (`Before=crio.service`, `RequiredBy=crio.service`)
  makes the store available before CRI-O starts.
- Rootless `skopeo` overlay + `virt-make-fs` produce a store CRI-O accepts.

Measure install time with and without `--bake-images` on first run (cold) and on
the second/third cluster of the same version (warm cache) to quantify the win.

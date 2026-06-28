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
3. `virt-make-fs --type=ext4 --label=baked-images --format=qcow2` packs the
   store into `store.qcow2`. `virt-make-fs` runs rootless via libguestfs, so no
   root is required.

Needs `skopeo` and `virt-make-fs` (guestfs-tools / libguestfs-tools) on PATH;
the bake stage preflights both.

### Multi-arch

The store is **multi-arch**. `SupportedReleaseArches` (`x86_64`, `aarch64`) are
each enumerated from their arch-specific release image
(`ocp-release:<version>-<arch>`) and unioned; an arch whose release image
doesn't exist for the version is skipped. One store therefore serves an amd64
node, an aarch64 node, and amd64 workloads run on aarch64 via Rosetta. The
RHCOS live-ISO arch is still selected separately (see
`providers/openshift.coreOSArch`) — baking does not change which ISO boots.

### Attach + wire (node side)

- `stages/createmastervms` uploads a **per-cluster** copy of `store.qcow2` into
  the libvirt pool (`ImportDisk`) and attaches it read-only + shareable. Per
  cluster — not shared — so `virsh undefine --remove-all-storage` on delete
  never strands another cluster.
- The disk is mounted by label (`/dev/disk/by-label/baked-images` →
  `/var/lib/baked-images`) and registered with CRI-O via
  `additionalimagestores` in a `storage.conf` drop-in.
- That wiring is applied in **both** install phases:
  - **post-pivot:** a master `MachineConfig` dropped into the install dir's
    `openshift/` (`Installer.WriteImageStoreManifest`) so it is rendered into
    the node's ignition and present from first boot, before CRI-O pulls
    operators.
  - **bootstrap:** the same file + mount unit merged into
    `bootstrap-in-place-for-live-iso.ign`
    (`Installer.MergeImageStoreIntoLiveISOIgnition`) before the ISO is embedded.

Renderers live in `providers/openshift/baker.go` (`RenderStorageConfDropin`,
`RenderMountUnit`, `RenderMachineConfig`, `MergeBakedStoreIntoIgnition`) and are
unit-tested in `baker_test.go`.

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

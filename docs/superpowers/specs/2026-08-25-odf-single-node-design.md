# `--odf`: OpenShift Data Foundation on a single-node cluster

Date: 2026-08-25
Status: approved design, pre-implementation
Source recipe: dfmicro's `odf-single-node-recipe.md` (MicroShift-targeted; this
spec is its OCP/easyshift translation). Credit: the single-node tricks —
`SINGLE_NODE=true`, the Immediate-binding StorageClass, the trimmed
StorageCluster — were worked out in dfmicro.

## Goal

`easyshift create -n foo --odf` produces an SNO cluster with a working ODF
(Ceph via Rook) storage stack: RBD + CephFS StorageClasses backed by a
dedicated VM data disk, sized for development, on both the libvirt and vfkit
backends.

Non-goals: multi-node ODF, real redundancy (replica 3 lands on one thin pool —
Ceph semantics, zero hardware redundancy), NooBaa/RGW/object storage
(explicitly disabled), offline operation (OLM needs the default catalogs; the
offline-install roadmap item notes the conflict).

## CLI + config

- `create --odf` (bool): enable the stack.
- `create --odf-disk <GB>` (int, default 100): size of the sparse backing data
  disk. Rejected by `validateNew` when set without `--odf`.
- `ClusterConfig` gains `ODF bool` and `ODFDiskGB int` (JSON-persisted).
- Usable capacity ≈ `ODFDiskGB/3 - overhead`; the per-OSD `dataPVCTemplate`
  request is `(ODFDiskGB/3) - 3` Gi (mon PVCs and thin-pool metadata come out
  of the same pool). Documented, not user-tunable.
- Preflight warning (not an error) when `MasterRAM < 16384`: the floored
  requests schedule, but Ceph below 16 GB thrashes.

## Storage backend: VM data disk + LVMS

dfmicro fakes a disk with an in-host loop device because MicroShift owns no
VM. easyshift owns the VM, so:

- `create-master-vms` attaches a second, **writable, per-cluster** extra disk
  when `ODF` is set, using the existing `interfaces.ExtraDisk` plumbing from
  image baking (`ReadOnly: false`, `Shareable: false`, so no struct changes).
  - libvirt: sparse qcow2 volume in the pool, per-cluster name
    (`easyshift-<cluster>-odf.qcow2`), removed with the domain's storage on
    delete.
  - vfkit: sparse raw file in the VM dir (`createDisk` already builds sparse
    raw files), removed with the VM dir on delete.
- Attach order is controlled and load-bearing: primary OS disk, then the bake
  store (if `--bake-images`, read-only), then the ODF data disk. The guest
  device path is computed from that order (`/dev/vdb` or `/dev/vdc`), and the
  `LVMCluster` selects it via `deviceSelector.paths`.
- The LVM Storage operator (LVMS, productized TopoLVM) builds the VG + thin
  pool in-node from an `LVMCluster` CR (thin-pool device class,
  `overprovision-ratio: 10`). No loop devices, no hand-run `vgcreate`, no
  modules-load.d MachineConfig — OCP's CSI drivers load `rbd`/`ceph`
  themselves.

## New stage: `install-odf`

Runs after `merge-kubeconfig`, before `finalize` (needs a working kubeconfig;
the API may still flap post-install, so every step goes through the existing
retry helper). The stage holds a new `interfaces.ODFInstaller`, implemented by
`providers/odf` (shells out to `oc` via `CommandRunner`), faked in
`providers/fakes`. All steps are idempotent (`oc apply` / merge-patch /
`--overwrite` label) so `create` resume re-runs them safely.

Ordered steps (recipe ordering, each wait-bounded):

1. **Operators**: namespaces + OperatorGroups + Subscriptions for
   `lvms-operator` (in the namespace its packagemanifest declares — LVMS has
   used both `openshift-storage` and `openshift-lvm-storage` across versions;
   the spike pins the right one for 4.22) and `odf-operator`
   (`openshift-storage`), both from the `redhat-operators` catalog, channel
   `stable-4.<minor>` derived from the cluster's OCP version. Wait for both
   CSVs `Succeeded` and for the `storageclusters.ocs.openshift.io` CRD.
2. **LVMCluster** pointing at the data disk device path; wait Ready. Then
   easyshift's own **Immediate-binding StorageClass** on `topolvm.io` with the
   device-class parameter (load-bearing: LVMS's default SC is
   WaitForFirstConsumer, which deadlocks mon/OSD PVCs).
3. **`SINGLE_NODE=true`** merge-patched onto the subscription whose
   `spec.name == ocs-operator` (found by `spec.name`, not `metadata.name` — it
   is created by odf-operator). Wait for the ocs-operator deployment to settle
   after the restart.
4. **Node label** `cluster.ocs.openshift.io/openshift-storage=` on all nodes
   (`--overwrite`).
5. **CephCSI `Driver` CRs** (`csi.ceph.io/v1`) for
   `openshift-storage.rbd.csi.ceph.com` and
   `openshift-storage.cephfs.csi.ceph.com`: `controllerPlugin.replicas: 1`,
   ~50m/100Mi sidecars. Applied unconditionally — easyshift's floor is OCP
   4.22, so ODF ≥ 4.19 always.
6. **Trimmed StorageCluster** exactly as the recipe renders it —
   monitoring/cephObjectStores/cephObjectStoreUsers/multiCloudGateway
   `reconcileStrategy: ignore`, every placement emptied (including the device
   set), every component request floored at 125m/128Mi, `monPVCTemplate` 2Gi
   on the Immediate SC, one `storageDeviceSet` `count: 1, replica: 3,
   portable: false`, block-mode `dataPVCTemplate` on the Immediate SC — with
   the derived size. Wait for `StorageCluster` phase `Ready` (timeout ~20 min).

Rollback: no-op (with comment). Justification: (a) the runner invokes
Rollback only on `easyshift delete` (failed creates resume, they don't roll
back), where the immediately following rollbacks destroy the VM and both
disks — no state survives that graceful in-cluster teardown could improve;
(b) attempting the recipe's finalizer-ordered removal during delete would
make `easyshift delete` depend on a live apiserver and healthy operators —
a half-broken cluster (the usual reason for deleting) could wedge the delete
indefinitely, and a no-op guarantees delete always completes. The recipe's
teardown ordering is only needed if a day-2 "remove ODF, keep the cluster"
command ever exists; that feature would inherit it
(StorageCluster → operators → VG/disk).

## Rendering + testing

- All manifests/patches are pure renderer functions in `providers/odf`
  (pattern: the baker's renderers), unit-tested for the load-bearing fields:
  Immediate binding mode, `SINGLE_NODE` env, `spec.name` subscription lookup,
  emptied placements, floored requests, derived PVC size, device path.
- Stage sequence tested against the fake ODFInstaller (call order pinned,
  same technique as generate-ignition's manifests-ordering test); app-level
  test asserts `--odf` wires disk + stage and plain creates touch neither.
- `--simulate` traces the ODF steps.
- Hardware validation: hand-run the sequence on the live `baked` cluster
  first (stop VM, hand-attach a data disk, start; then apply steps 1–6 with
  `oc`) to confirm the OCP translation before coding; then one real
  `create -n odftest --odf` end-to-end after implementation.

## Risks / open items

- **Memory**: ODF on a 16 GB SNO is tight even floored; the preflight warning
  plus docs recommend 20 GB+ where the host allows.
- **Catalog dependency**: requires the default OperatorHub catalogs (online).
  The offline-install roadmap item disables exactly those; if both features
  are wanted together later, the catalogs must be mirrored into the bake.
- **Channel derivation**: `stable-4.<minor>` assumed present for both
  operators at the cluster's minor; validated on hardware for 4.22 during the
  spike.
- **Device naming**: virtio disk order → `/dev/vdX` assumed stable across
  reboots on both backends (single controller, fixed attach order). Verified
  during the spike.

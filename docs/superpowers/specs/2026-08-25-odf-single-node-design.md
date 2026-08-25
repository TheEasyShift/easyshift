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
- Resource floors (spike-validated): `--odf` requires 8 vCPUs and 19456 MiB
  master RAM. The manager raises `MasterCPUs` to 8 and `MasterRAM` to 19456
  when `--odf` is set and the user left them lower, logging the bump;
  `create` also gains `--master-cpus` (default 4) so users can go higher.

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
   `openshift-storage.cephfs.csi.ceph.com`: `controllerPlugin.replicas: 1`
   and floors for ALL controller containers (plugin 100Mi/50m, sidecars
   50Mi/25m each, omapGenerator 50Mi/10m, addons 32Mi/10m — see spike
   results; the defaults total ~900Mi per controller pod). Applied
   unconditionally and BEFORE the StorageCluster, so the CSI pods are born
   small (mid-flight trims can deadlock: the csi-operator itself becomes
   unschedulable on a full node). A `cluster-monitoring-config` trim
   (prometheus 300Mi/4h retention, other components ~50Mi) is applied in the
   same step.
6. **Trimmed StorageCluster** — the recipe with three 4.22 deltas (see spike
   results): monitoring/cephObjectStores/cephObjectStoreUsers/multiCloudGateway
   `reconcileStrategy: ignore`; component placements (mon/mds/mgr/…) emptied;
   component requests floored at 125m/128Mi; `monPVCTemplate` 2Gi on the
   Immediate SC; one `storageDeviceSet` with **`count: 3, replica: 1`**,
   `portable: false`, block-mode `dataPVCTemplate` with the derived size, and
   non-empty `placement` + `preparePlacement` carrying the no-op
   TopologySpreadConstraint (`kubernetes.io/hostname`, `ScheduleAnyway`).
   Wait for `StorageCluster` phase `Ready` (timeout 30 min), then for the
   `ocs-storagecluster-ceph-rbd` and `ocs-storagecluster-cephfs`
   StorageClasses (created asynchronously by ocs-client-operator).

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

- **Memory**: 8 vCPU / 19456 MiB is the validated floor (97% memory
  requested). A 24 GB host is the practical minimum and has zero headroom;
  docs say so plainly. 20 GB+ VMs need a 28 GB+ host.
- **Catalog dependency**: requires the default OperatorHub catalogs (online).
  The offline-install roadmap item disables exactly those; if both features
  are wanted together later, the catalogs must be mirrored into the bake.
- **Channel derivation**: `stable-4.<minor>` assumed present for both
  operators at the cluster's minor; validated on hardware for 4.22 during the
  spike.
- **Device naming**: virtio disk order → `/dev/vdX` assumed stable across
  reboots on both backends (single controller, fixed attach order). Verified
  during the spike.

## Spike results (2026-08-25, `baked` cluster, OCP 4.22.9 / ODF 4.22.2 / LVMS 4.22.0)

Full stack reached StorageCluster `Ready` + `HEALTH_OK`, 3 OSDs, RBD and
CephFS StorageClasses, and a PVC write test succeeded. Pinned facts:

- Device path: third virtio disk = `/dev/vdc` with `--bake-images`, `/dev/vdb`
  without. Attach order is stable across reboots.
- Operators: both `lvms-operator` and `odf-operator` in `redhat-operators`,
  channel `stable-4.22`; namespaces `openshift-lvm-storage` /
  `openshift-storage`. `odf-operator` spawns ~11 dependent subscriptions; the
  stage must wait for the `ocs-operator` CSV (`Succeeded`) and the
  `storageclusters.ocs.openshift.io` CRD, not just the odf-operator CSV. The
  ocs subscription's metadata.name is
  `ocs-operator-stable-4.22-redhat-operators-openshift-marketplace` — find it
  by `spec.name`.
- `LVMCluster` (`lvm.topolvm.io/v1alpha1`) with a thin-pool deviceClass on the
  device path reaches `status.state: Ready` in ~2 min and builds VG+thin pool
  in-node. The Immediate SC (`topolvm.io`, `topolvm.io/device-class` param)
  provisions mon/OSD PVCs correctly.
- `SINGLE_NODE=true` env (read via `util.IsSingleNodeDeployment`) works, BUT
  three recipe deltas are REQUIRED on ODF 4.22:
  1. Device set must be `count: 3, replica: 1` (not `replica: 3`):
     `getMinimumNodes` takes `max(deviceSet.replica)` AFTER the SINGLE_NODE
     relaxation, so `replica: 3` re-raises the node requirement to 3.
  2. Empty placements don't work for OSDs: deviceSet `placement: {}` is
     ignored (`isPlacementEmpty` → defaults win), and the default renders a
     TopologySpreadConstraint from the failure-domain key, which is EMPTY in
     SINGLE_NODE mode (`failureDomain=osd` has no key) → osd-prepare Jobs are
     invalid ("topologyKey: Required value"). Setting `spec.placement.osd: {}`
     at the StorageCluster level would panic the operator
     (`TopologySpreadConstraints[0]` on an empty default). The fix: give the
     deviceSet non-empty `placement` AND `preparePlacement`, each with a
     valid no-op TSC (`topologyKey: kubernetes.io/hostname`,
     `whenUnsatisfiable: ScheduleAnyway`, maxSkew 1, labelSelector
     `ceph.rook.io/pvc Exists`) — `mergePlacements` substitutes it wholesale.
  3. The CephCSI `Driver` CRs must floor ALL controller containers, not just
     the four sidecars: valid resource keys are `plugin` (default 250Mi!),
     `omapGenerator` (125Mi), `addons`, `liveness`, `logRotator`, `attacher`,
     `provisioner`, `resizer`, `snapshotter`. Trimmed values that work:
     plugin 100Mi/50m, sidecars 50Mi/25m, omapGenerator 50Mi/10m,
     addons 32Mi/10m. These must be applied BEFORE the StorageCluster: the
     csi-operator (`ceph-csi-controller-manager`) can itself become
     unschedulable on a full node, deadlocking the trim it would apply.
- StorageCluster `Ready` is not the end: the RBD/CephFS StorageClasses are
  created asynchronously by ocs-client-operator (StorageClient `Connected`).
  The stage must wait for the two SCs (`ocs-storagecluster-ceph-rbd`,
  `ocs-storagecluster-cephfs`) explicitly.
- Resources: 4 vCPU/16 GB is structurally insufficient (99% CPU requested
  before Ceph starts). Working configuration: **8 vCPUs, 19456 MiB** → 97%
  memory requested with the trims above. A cluster-monitoring-config trim
  (prometheus 300Mi etc.) is part of the fit. Host note: a 20 GB VM thrashes
  a 24 GB Mac into a load-300 death spiral; 19 GB is the ceiling there.
- Timeouts observed: CSVs ≤ 10 min, LVMCluster ≤ 2 min, StorageCluster
  Ready 15–45 min (budget 30 min on a right-sized VM), SCs + a few min.

## Implementation validation (2026-08-25, cluster `odfy`, shipped code path)

`easyshift create -n odfy --odf --master-disk 60 --master-ram 19456` (no
baking → `/dev/vdb` path), zero hand-edits: cluster converged in ~26 min and
the `install-odf` stage completed all four phases in **~13 min** —
StorageCluster `Ready` ~6 min after creation (the spike's 45 min was
debugging, not inherent cost). Verified: Ceph `HEALTH_OK`, 3 OSDs, both
`ocs-storagecluster-ceph-rbd`/`-cephfs` StorageClasses, RBD PVC Bound in
21 s with a pod write (`odf-works`), and a full `easyshift stop`/`start`
cycle after which the node returned Ready, Ceph `HEALTH_OK` with all OSDs,
and the StorageCluster re-settled to `Ready` (device paths stable across
reboots). CPU auto-raise observed in the log ("raising master CPUs from 4").

Two findings outside --odf's scope, recorded for follow-up: (1) the
`--master-ram` default (32768) exceeds Virtualization.framework's cap on a
24 GB host — vfkit crash-loops at launch and the install watchdog then
flips the phase to "run" on the dead VM; create must be given an explicit
`--master-ram` on small hosts until a host-RAM-aware cap/preflight exists.
(2) The vmnet-helper sidecar dies with the invoking process group while
vfkit survives, leaving a running VM with no network; `start` on a running
VM does not respawn a dead sidecar. Both are pre-existing macOS backend
gaps, tracked in ROADMAP.md.

## Policy revision (2026-08-25, post-merge of PR #19)

Decision: **trim, never disable.** The shipped StorageCluster no longer sets
any `reconcileStrategy: ignore` — NooBaa, Ceph RGW, and ODF monitoring all
reconcile normally with the recipe's 125m/128Mi floors applied to their
components. The single-node fit remains resource trims + emptied placements
only. Consequence: the RAM floor rises from 19456 to 24576 MiB (requests grow
~1.3 GiB and NooBaa's real usage runs 1.5–2 GiB above its floors), which
means `--odf` no longer fits a 24 GB host; validated targets are a 36 GB
macOS host and larger Linux hosts. Insights stays enabled (the disable
applied to cluster `odfy` during debugging was a one-off, not product
behavior). The earlier ROADMAP idea of `--odf-profile` remains the future
home for both a smaller dev tier and ODF's native lean/balanced profiles.

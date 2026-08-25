# `--odf`: OpenShift Data Foundation internals

Design: [docs/superpowers/specs/2026-08-25-odf-single-node-design.md](../superpowers/specs/2026-08-25-odf-single-node-design.md).
Credit: the single-node tricks this stage is built on —
`SINGLE_NODE=true`, the Immediate-binding StorageClass, the trimmed
StorageCluster — were worked out in dfmicro's `odf-single-node-recipe.md`
(MicroShift-targeted); this stage is its OCP/easyshift translation, plus
three deltas required on ODF 4.22 (below).

`--odf` installs a working Ceph-via-Rook storage stack (RBD + CephFS
StorageClasses) onto the single master node, backed by a dedicated VM data
disk and an in-node LVMS thin pool. No loop devices, no hand-run `vgcreate`,
no `modules-load.d` MachineConfig — OCP's CSI drivers load `rbd`/`ceph`
themselves.

## The data disk and device-path scheme

`stages/createmastervms` attaches a second, writable, per-cluster extra disk
when `Cluster.ODF` is set, reusing the `interfaces.ExtraDisk` plumbing built
for image baking (`ReadOnly: false`, `Shareable: false` — no struct changes
needed):

- **libvirt**: a sparse qcow2 volume in the pool, named
  `easyshift-<cluster>-odf.qcow2`, removed with the domain's storage on
  delete.
- **vfkit**: a sparse raw file in the VM dir, removed with the VM dir on
  delete.

Attach order is controlled and load-bearing: primary OS disk, then the bake
store (if `--bake-images`, read-only), then the ODF data disk. The guest
device path follows that order — `stages/installodf/stage.go`'s `spec()`
picks it directly from `Cluster.BakeImages`:

```go
device := "/dev/vdb"
if sc.Cluster.BakeImages {
    device = "/dev/vdc" // the bake store occupies vdb
}
```

`LVMCluster`'s `deviceSelector.paths` points at that path. Verified stable
across reboots on both backends during the hardware spike (single virtio
controller, fixed attach order).

## The `install-odf` stage: four phases

Runs after `merge-kubeconfig`, before `finalize` — it needs a working
kubeconfig, and the API may still flap post-install, so every mutating step
goes through a retrying poll (`pollUntil` in `providers/odf/installer.go`).
`stages/installodf.Stage` is a thin driver over `interfaces.ODFInstaller`
(implemented by `providers/odf.Installer`, which shells out to `oc` via
`CommandRunner`; faked in `providers/fakes`). No-op when `Cluster.ODF` is
false. Every step is `oc apply` / merge-patch / `--overwrite` label, so
`create` resume re-runs the stage safely.

1. **`InstallOperators`** — applies namespaces + OperatorGroups +
   Subscriptions for `lvms-operator` (`openshift-lvm-storage`) and
   `odf-operator` (`openshift-storage`), both from `redhat-operators` at
   channel `stable-4.<minor>` (`config.OLMChannelForVersion`). Waits for the
   `lvms-operator` CSV, the `ocs-operator` CSV (odf-operator spawns ~11
   dependent subscriptions; ocs-operator's arrives among them — waiting on
   odf-operator's own CSV alone isn't enough), and the
   `storageclusters.ocs.openshift.io` CRD.
2. **`SetupLVM`** — applies the `LVMCluster` CR pointed at the device path,
   waits for `status.state: Ready` (~2 min on the spike hardware; LVMS
   builds the VG + thin pool in-node), then applies easyshift's own
   **Immediate-binding StorageClass** (`easyshift-odf-immediate`,
   provisioner `topolvm.io`). This is load-bearing: LVMS's own default
   StorageClass is `WaitForFirstConsumer`, which deadlocks the mon/OSD PVCs
   ODF creates before any pod that would trigger binding is scheduled.
3. **`EnableSingleNode`** — finds the ocs-operator Subscription by
   `spec.name == "ocs-operator"` (its `metadata.name` is catalog-decorated,
   e.g. `ocs-operator-stable-4.22-redhat-operators-openshift-marketplace`,
   and it's created by odf-operator, not hand-authored), merge-patches
   `SINGLE_NODE=true` onto it, waits for the resulting ocs-operator
   deployment restart to report the env var and `Available`, labels all
   nodes `cluster.ocs.openshift.io/openshift-storage=`, then applies the
   CephCSI `Driver` CRs and the monitoring trim (delta 3 below).
4. **`CreateStorageCluster`** — applies the trimmed `StorageCluster` CR
   (delta 1 + 2 below), waits for `status.phase: Ready` (budget 30 min; the
   spike saw 15–45 min), then waits for `ocs-client-operator` to
   asynchronously create both `ocs-storagecluster-ceph-rbd` and
   `ocs-storagecluster-cephfs` StorageClasses — `Ready` is not the end of
   the story.

## The three ODF 4.22 recipe deltas

dfmicro's recipe targets MicroShift; three points needed correction for
OCP/ODF 4.22, all found and confirmed on real hardware during the spike
(2026-08-25, OCP 4.22.9 / ODF 4.22.2 / LVMS 4.22.0):

**1. Device set `count: 3, replica: 1`, not `replica: 3`.**
`getMinimumNodes` computes `max(deviceSet.replica)` *after* the
`SINGLE_NODE` relaxation has already lowered the requirement — so a device
set left at `replica: 3` re-raises the node requirement straight back to 3
and the StorageCluster never converges on one node. `replica: 1` (with
`count: 3`, i.e. three OSDs on the one node) keeps it at 1.

**2. Non-empty no-op TSC placements, not empty placements.**
The natural way to say "don't constrain scheduling on a single node" is an
empty `placement: {}` on the device set — but `isPlacementEmpty` treats `{}`
as "no override" and lets the *default* placement win, and that default
renders a `TopologySpreadConstraint` keyed on the failure-domain label. Under
`SINGLE_NODE`, `failureDomain=osd` carries no such label, so the rendered TSC
has an empty `topologyKey` and every osd-prepare Job fails admission
(`topologyKey: Required value`). Setting `spec.placement.osd: {}` at the
StorageCluster level doesn't help either — it panics the operator indexing
`TopologySpreadConstraints[0]` off an empty default. The fix
(`providers/odf/render.go`'s `deviceSetTSC`): give the device set **non-empty**
`placement` *and* `preparePlacement`, each carrying a valid, always-satisfied
TopologySpreadConstraint (`topologyKey: kubernetes.io/hostname`,
`whenUnsatisfiable: ScheduleAnyway`, `maxSkew: 1`, `labelSelector:
ceph.rook.io/pvc Exists`). `mergePlacements` substitutes it wholesale instead
of falling back to the broken default.

**3. Floor every CephCSI `Driver` controller container, and apply before the
StorageCluster.** The recipe trims only the four sidecars (attacher /
provisioner / resizer / snapshotter). On 4.22 that's not enough: the `plugin`
container alone defaults to a 250Mi request, and `omapGenerator` / `addons`
have their own defaults too — untrimmed, a single controller pod requests
~900Mi. `RenderDriver` floors all of them: `plugin` 100Mi/50m, each sidecar
50Mi/25m, `omapGenerator` 50Mi/10m, `addons` 32Mi/10m. Applied — together with
a `cluster-monitoring-config` trim (Prometheus 300Mi/4h retention, other
monitoring components ~50Mi each) — **before** the StorageCluster is created,
not as a follow-up patch: on a full node the csi-operator
(`ceph-csi-controller-manager`) can itself become unschedulable, which would
deadlock a trim applied after the fact.

## Resource floors

`app/manager.go`'s `validateNew` raises `MasterCPUs` to 8 and `MasterRAM` to
19456 MiB whenever `--odf` is set and the user left them lower (logging the
bump); `create --master-cpus` (default 4) lets a user go higher up front. The
floor is spike-validated, not a guess: 4 vCPU/16 GB is structurally
insufficient (99% CPU requested before Ceph even starts). 8 vCPU/19456 MiB
converges at ~97% memory requested with all the trims above applied — there
is effectively no headroom left in that configuration. That's a floor, not
a ceiling: a bigger `--master-ram` scales the same way it does without
`--odf` — a 20 GB+ master VM needs a 28 GB+ host, since the 19456 MiB floor
already assumes almost no free host memory beyond the VM itself.

## Capacity math

Usable Ceph capacity is roughly `ODFDiskGB / 3` — three OSDs share the one
data disk, so PV-visible space is about a third of the disk regardless of
replica count (replica 1 here is a single copy per OSD, not 3x
duplication — all three OSDs still sit on the same physical disk, so this
buys none of Ceph's usual hardware redundancy: losing that one disk loses
everything, the same caveat [docs/user/usage.md](../user/usage.md) makes for
users). The per-OSD
`dataPVCTemplate` request the stage computes
(`stages/installodf/stage.go`'s `spec()`) is `ODFDiskGB/3 - 3` Gi: the `-3`
leaves room in the same thin pool for the 2Gi mon PVC and thin-pool metadata
overhead. This math is documented, not user-tunable — there's no flag for
the per-OSD size, only for the whole disk (`--odf-disk`).

## The Immediate-binding StorageClass trick

ODF's mon and OSD PVCs are created by the operator before any pod exists
that would trigger `WaitForFirstConsumer` binding — on a normal cluster that
consumer is implicit (a scheduler decision across nodes), but there's only
one node here and nothing schedules first. easyshift's own
`easyshift-odf-immediate` StorageClass (provisioner `topolvm.io`,
`volumeBindingMode: Immediate`) sits directly on top of LVMS/TopoLVM and is
used for every PVC template the StorageCluster renders (mon and OSD alike),
sidestepping the deadlock entirely.

## Rollback: no-op, and why

`Stage.Rollback` does nothing. Two reasons:

1. The `Runner` only invokes `Rollback` on `easyshift delete` — a failed
   `create` resumes rather than rolling back — and `delete`'s subsequent
   stage rollbacks destroy the VM and both disks moments later. No amount of
   graceful in-cluster Ceph teardown improves on that outcome; there's
   nothing left to protect.
2. Doing the recipe's real teardown (finalizer-ordered: StorageCluster then
   operators then VG/disk) during `delete` would make `delete` depend on a
   live apiserver and healthy operators. Since a broken cluster is a common
   reason to delete one, that dependency could wedge `delete` indefinitely.
   A no-op guarantees `delete` always completes.

The ordered teardown the recipe describes is only needed for a *keep-the-
cluster* removal — see the `easyshift odf remove` day-2 item in
[ROADMAP.md](../../ROADMAP.md).

## Rendering + testing

Every manifest/patch is a pure string-builder function in `providers/odf`
(`render.go`), the same pattern as the image baker's renderers — no I/O, no
cluster access. Each is unit-tested for its load-bearing field: Immediate
binding mode, the `SINGLE_NODE` env value, the `spec.name` subscription
lookup, the emptied StorageCluster component placements, the floored
resource requests, the derived PVC size, and the device path selection.
`stages/installodf`'s call order against the fake `ODFInstaller` is pinned
(same technique as `generate-ignition`'s manifest-ordering test); an
app-level test asserts `--odf` wires both the data disk and this stage, and
that plain creates touch neither. `--simulate` traces all four phases.

## Verification boundary

Hand-validated end-to-end on real hardware ahead of implementation (stop the
`baked` VM, hand-attach a data disk, start it, then apply steps 1–6 with
`oc`) to confirm the OCP translation before coding, then again with a real
`easyshift create -n odftest --odf` after. The spike reached StorageCluster
`Ready`, `HEALTH_OK`, 3 OSDs, both StorageClasses, and a PVC write test.
What still needs a live cluster to reconfirm on future changes: CSV/CRD
timing, `LVMCluster` convergence, the `SINGLE_NODE` restart settling, and the
asynchronous StorageClass creation — none of that is simulate-able against
fakes.

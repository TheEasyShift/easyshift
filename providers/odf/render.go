// Package odf holds the pure manifest/patch renderers for the --odf storage
// stack (LVMS + ODF/Rook-Ceph on a single node). Every function here is a
// pure string builder — no I/O, no cluster access — mirroring the pattern in
// providers/openshift/baker.go. Applying these manifests is the job of the
// install-odf stage's ODFInstaller, not this package.
package odf

import "fmt"

// ImmediateStorageClassName is easyshift's own Immediate-binding StorageClass
// on top of LVMS/TopoLVM. Load-bearing: LVMS's default StorageClass is
// WaitForFirstConsumer, which deadlocks the mon/OSD PVCs ODF creates before
// any pod that would trigger binding is scheduled.
const ImmediateStorageClassName = "easyshift-odf-immediate"

// RBDStorageClass and CephFSStorageClass are the StorageClasses
// ocs-client-operator creates asynchronously once the StorageCluster is
// Ready. The install-odf stage waits for both to exist before finishing.
const (
	RBDStorageClass    = "ocs-storagecluster-ceph-rbd"
	CephFSStorageClass = "ocs-storagecluster-cephfs"
)

// Pinned by the 2026-08-25 hardware spike (OCP 4.22.9 / ODF 4.22.2 / LVMS
// 4.22.0): the namespaces, OperatorGroup names, and Subscription names LVMS
// and ODF land in on this OCP series.
const (
	lvmsNamespace = "openshift-lvm-storage"
	lvmsOGName    = "lvms-operator-group"
	lvmsSubName   = "lvms-operator"

	odfNamespace = "openshift-storage"
	odfOGName    = "openshift-storage-operator-group"
	odfSubName   = "odf-operator"

	catalogSource          = "redhat-operators"
	catalogSourceNamespace = "openshift-marketplace"
)

// lvmDeviceClass is the name of the single LVMCluster device class / TopoLVM
// device-class parameter used throughout the stack.
const lvmDeviceClass = "odf"

// RenderOperators returns the namespaces, OperatorGroups, and Subscriptions
// for lvms-operator (openshift-lvm-storage) and odf-operator
// (openshift-storage), both from the redhat-operators catalog at channel.
// The install-odf stage waits for both CSVs Succeeded and the
// storageclusters.ocs.openshift.io CRD before moving on.
func RenderOperators(channel string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: %[2]s
  namespace: %[1]s
spec:
  targetNamespaces:
  - %[1]s
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: %[3]s
  namespace: %[1]s
spec:
  channel: %[5]s
  name: %[3]s
  source: %[6]s
  sourceNamespace: %[7]s
  installPlanApproval: Automatic
---
apiVersion: v1
kind: Namespace
metadata:
  name: %[4]s
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: %[8]s
  namespace: %[4]s
spec:
  targetNamespaces:
  - %[4]s
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: %[9]s
  namespace: %[4]s
spec:
  channel: %[5]s
  name: %[9]s
  source: %[6]s
  sourceNamespace: %[7]s
  installPlanApproval: Automatic
`, lvmsNamespace, lvmsOGName, lvmsSubName, odfNamespace, channel, catalogSource, catalogSourceNamespace, odfOGName, odfSubName)
}

// RenderLVMCluster returns the LVMCluster CR that builds a VG + thin pool
// in-node on devicePath (the guest device path of the ODF data disk, e.g.
// /dev/vdc). Reaches status.state Ready in ~2 min per the spike.
func RenderLVMCluster(devicePath string) string {
	return fmt.Sprintf(`apiVersion: lvm.topolvm.io/v1alpha1
kind: LVMCluster
metadata:
  name: easyshift-odf
  namespace: %s
spec:
  storage:
    deviceClasses:
    - name: %s
      default: true
      fstype: xfs
      deviceSelector:
        paths:
        - %s
      thinPoolConfig:
        name: thin
        overprovisionRatio: 10
        sizePercent: 90
`, lvmsNamespace, lvmDeviceClass, devicePath)
}

// RenderImmediateStorageClass returns easyshift's own Immediate-binding
// StorageClass on top of TopoLVM. See ImmediateStorageClassName.
func RenderImmediateStorageClass() string {
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: topolvm.io
parameters:
  csi.storage.k8s.io/fstype: xfs
  topolvm.io/device-class: %s
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
`, ImmediateStorageClassName, lvmDeviceClass)
}

// RenderDriver returns the CephCSI Driver CR (csi.ceph.io/v1) for name (one
// of openshift-storage.rbd.csi.ceph.com / openshift-storage.cephfs.csi.ceph.com),
// applied BEFORE the StorageCluster so the CSI controller pods are born
// small. Per the spike, the default plugin container alone requests 250Mi;
// ALL controller containers must be floored, not just the four sidecars
// (attacher/provisioner/resizer/snapshotter), or the trims don't fit an
// 8vCPU/24576Mi node:
//   - plugin: 100Mi / 50m
//   - attacher, provisioner, resizer, snapshotter: 50Mi / 25m each
//   - omapGenerator: 50Mi / 10m
//   - addons: 32Mi / 10m
func RenderDriver(name string) string {
	return fmt.Sprintf(`apiVersion: csi.ceph.io/v1
kind: Driver
metadata:
  name: %s
  namespace: %s
spec:
  controllerPlugin:
    replicas: 1
    resources:
      plugin:
        requests:
          memory: 100Mi
          cpu: 50m
      attacher:
        requests:
          memory: 50Mi
          cpu: 25m
      provisioner:
        requests:
          memory: 50Mi
          cpu: 25m
      resizer:
        requests:
          memory: 50Mi
          cpu: 25m
      snapshotter:
        requests:
          memory: 50Mi
          cpu: 25m
      omapGenerator:
        requests:
          memory: 50Mi
          cpu: 10m
      addons:
        requests:
          memory: 32Mi
          cpu: 10m
`, name, odfNamespace)
}

// RenderMonitoringTrim returns the cluster-monitoring-config ConfigMap that
// floors the platform monitoring stack's resource requests so it fits
// alongside ODF on a single node. Applied in the same step as the Driver CRs,
// before the StorageCluster.
func RenderMonitoringTrim() string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-monitoring-config
  namespace: openshift-monitoring
data:
  config.yaml: |
    prometheusK8s:
      retention: 4h
      resources:
        requests:
          memory: 300Mi
          cpu: 50m
    alertmanagerMain:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    prometheusOperator:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    metricsServer:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    kubeStateMetrics:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    openshiftStateMetrics:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    telemeterClient:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
    monitoringPlugin:
      resources:
        requests:
          memory: 50Mi
          cpu: 10m
`
}

// deviceSetTSC is the no-op TopologySpreadConstraint substituted wholesale
// for both the deviceSet's placement and preparePlacement. Per the spike, an
// empty placement ({}) is ignored by isPlacementEmpty, so the default
// TopologySpreadConstraint wins — and the default's topologyKey is empty
// under SINGLE_NODE (failureDomain=osd has no key), which makes osd-prepare
// Jobs invalid ("topologyKey: Required value"). This valid, always-satisfied
// TSC keeps mergePlacements happy without actually constraining anything on
// a one-node cluster.
const deviceSetTSC = `      topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: ScheduleAnyway
        labelSelector:
          matchExpressions:
          - key: ceph.rook.io/pvc
            operator: Exists
`

// RenderStorageCluster returns the single-node StorageCluster. Production
// features (NooBaa, RGW, ODF monitoring) all reconcile normally — the
// single-node fit comes from floored resource requests and emptied
// placements, never from disabling features. The three hardware-validated
// 4.22 deltas remain: count/replica (getMinimumNodes), non-empty no-op TSC
// placements, and the Driver CR floors applied before this manifest.
func RenderStorageCluster(pvcGi int) string {
	return fmt.Sprintf(`apiVersion: ocs.openshift.io/v1
kind: StorageCluster
metadata:
  name: ocs-storagecluster
  namespace: %[1]s
spec:
  monPVCTemplate:
    spec:
      storageClassName: %[2]s
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 2Gi
  placement:
    mon: {}
    mds: {}
    mgr: {}
    rbd-mirror: {}
    rgw: {}
    nfs: {}
    noobaa-core: {}
    noobaa-standalone: {}
    osd-prepare: {}
  resources:
    mon:
      requests:
        cpu: 125m
        memory: 128Mi
    mds:
      requests:
        cpu: 125m
        memory: 128Mi
    mgr:
      requests:
        cpu: 125m
        memory: 128Mi
    mgr-sidecar:
      requests:
        cpu: 125m
        memory: 128Mi
    nfs:
      requests:
        cpu: 125m
        memory: 128Mi
    noobaa-core:
      requests:
        cpu: 125m
        memory: 128Mi
    noobaa-db:
      requests:
        cpu: 125m
        memory: 128Mi
    noobaa-db-vol:
      requests:
        storage: 5Gi
    noobaa-endpoint:
      requests:
        cpu: 125m
        memory: 128Mi
    rbd-mirror:
      requests:
        cpu: 125m
        memory: 128Mi
    rgw:
      requests:
        cpu: 125m
        memory: 128Mi
  storageDeviceSets:
  - count: 3
    name: ocs-deviceset
    dataPVCTemplate:
      spec:
        storageClassName: %[2]s
        accessModes:
        - ReadWriteOnce
        resources:
          requests:
            storage: %[3]dGi
        volumeMode: Block
    placement:
%[4]s    preparePlacement:
%[4]s    portable: false
    replica: 1
    resources:
      requests:
        cpu: 125m
        memory: 128Mi
`, odfNamespace, ImmediateStorageClassName, pvcGi, deviceSetTSC)
}

// SingleNodePatch returns the JSON merge patch that flips the ocs-operator
// Subscription (found by spec.name == "ocs-operator", not metadata.name —
// odf-operator creates it) into single-node mode. Read by ocs-operator via
// util.IsSingleNodeDeployment; restarts the operator pod, so the install-odf
// stage waits for the deployment to settle before applying the
// StorageCluster.
func SingleNodePatch() string {
	return `{"spec":{"config":{"env":[{"name":"SINGLE_NODE","value":"true"}]}}}`
}

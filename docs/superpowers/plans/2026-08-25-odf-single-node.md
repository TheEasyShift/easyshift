# `--odf` Single-Node ODF Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `easyshift create -n foo --odf` produces an SNO cluster with a Ready ODF StorageCluster (RBD + CephFS StorageClasses) backed by a dedicated VM data disk.

**Architecture:** A writable per-cluster data disk is attached at create time; a new `install-odf` stage (after `merge-kubeconfig`) drives a new `providers/odf` installer through four ordered phases: operators → LVM/StorageClass → single-node enablement → StorageCluster. All manifests are pure renderer functions; the installer shells out to `oc` via `CommandRunner`.

**Tech Stack:** Go (existing easyshift layout: `config ← interfaces ← stages/providers ← app ← cmd`), OLM (`redhat-operators`), LVMS, ODF 4.22, `oc` CLI.

**Spec:** `docs/superpowers/specs/2026-08-25-odf-single-node-design.md` — read it first; its "Spike results" section is the source of every magic value below.

## Global Constraints

- SNO only; every value below is hardware-validated on OCP 4.22.9 / ODF 4.22.2 / LVMS 4.22.0.
- `--odf` floors: `MasterCPUs >= 8`, `MasterRAM >= 19456` MiB (manager raises + logs, never errors), `ODFDiskGB` default 100.
- Device path: `/dev/vdc` when `BakeImages`, else `/dev/vdb` (attach order: OS disk, bake store, ODF disk).
- StorageCluster deviceSet MUST be `count: 3, replica: 1` with non-empty `placement` AND `preparePlacement` (no-op TSC on `kubernetes.io/hostname`, `ScheduleAnyway`). Never `replica: 3`, never `{}`, never StorageCluster-level `osd: {}` (operator panic).
- Driver CRs floor ALL controller containers (plugin 100Mi/50m, attacher/provisioner/resizer/snapshotter 50Mi/25m, omapGenerator 50Mi/10m, addons 32Mi/10m) and are applied BEFORE the StorageCluster.
- The ocs subscription is found by `spec.name == "ocs-operator"`, never by metadata.name.
- Stage completion = StorageCluster `Ready` AND both `ocs-storagecluster-ceph-rbd` + `ocs-storagecluster-cephfs` StorageClasses exist.
- Makefile is the build entry: `make check` must pass at the end of every task. Commits use `git commit -s` and end with `Assisted-by: Claude Code/claude-fable-5`.

---

### Task 1: Config + CLI surface

**Files:**
- Modify: `config/config.go` (ClusterConfig fields)
- Modify: `config/paths.go` (ODF vol name helper)
- Modify: `cmd/easyshift/main.go` (flags)
- Modify: `app/manager.go` (defaults + validation)
- Test: `app/cluster_test.go`

**Interfaces:**
- Produces: `ClusterConfig.ODF bool`, `ClusterConfig.ODFDiskGB int`, `config.ODFVolName(name string) string`, create flags `--odf`, `--odf-disk`, `--master-cpus`.

- [ ] **Step 1: Write the failing test** (append to `app/cluster_test.go`)

```go
// TestValidateODFDefaults asserts --odf raises the resource floors and
// defaults the data disk, and that --odf-disk without --odf is rejected.
func TestValidateODFDefaults(t *testing.T) {
	cfg, deps, _ := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("odfy")
	c.ODF = true
	c.MasterCPUs = 4
	c.MasterRAM = 16000
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.MasterCPUs != 8 || c.MasterRAM != 19456 || c.ODFDiskGB != 100 {
		t.Errorf("ODF floors not applied: cpus=%d ram=%d disk=%d", c.MasterCPUs, c.MasterRAM, c.ODFDiskGB)
	}

	bad := newTestCluster("odfless")
	bad.ODFDiskGB = 50 // set without ODF
	if err := mgr.Create(context.Background(), bad); err == nil {
		t.Error("expected error: --odf-disk without --odf")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestValidateODFDefaults`
Expected: FAIL (unknown fields `ODF`/`ODFDiskGB`)

- [ ] **Step 3: Implement**

`config/config.go`, next to `BakeImages`:

```go
	// ODF, when true, installs OpenShift Data Foundation after the cluster
	// converges: a dedicated data disk, LVMS thin pool, and a single-node
	// Ceph StorageCluster. See docs/dev/odf.md.
	ODF bool `json:"odf,omitempty"`
	// ODFDiskGB is the sparse backing data disk size (GB). Usable Ceph
	// capacity is about a third of this. 0 means the default (100).
	ODFDiskGB int `json:"odfDiskGB,omitempty"`
```

`config/paths.go`, next to `ImageStoreVolName` (reuses `imageStoreDiskExt`):

```go
// ODFVolName is the per-cluster name of the ODF data disk: a libvirt pool
// volume on Linux, a file in the vfkit state dir on macOS.
func ODFVolName(name string) string {
	return "easyshift-" + name + "-odf." + imageStoreDiskExt()
}
```

`app/manager.go`: add constants near `defaultMasterDiskGB`:

```go
	defaultODFDiskGB = 100
	odfMinCPUs       = 8
	odfMinRAMMiB     = 19456
```

In the defaults-filling block (where `MasterDiskGB` is defaulted):

```go
	if c.ODF {
		if c.MasterCPUs < odfMinCPUs {
			logrus.Infof("--odf requires %d vCPUs; raising master CPUs from %d", odfMinCPUs, c.MasterCPUs)
			c.MasterCPUs = odfMinCPUs
		}
		if c.MasterRAM < odfMinRAMMiB {
			logrus.Infof("--odf requires %d MiB RAM; raising master RAM from %d", odfMinRAMMiB, c.MasterRAM)
			c.MasterRAM = odfMinRAMMiB
		}
		if c.ODFDiskGB == 0 {
			c.ODFDiskGB = defaultODFDiskGB
		}
	}
```

In `validateNew`:

```go
	if !c.ODF && c.ODFDiskGB != 0 {
		return fmt.Errorf("--odf-disk requires --odf")
	}
```

`cmd/easyshift/main.go`: add vars `odf bool`, `odfDisk int`, `masterCPUs int`; literal fields `ODF: odf, ODFDiskGB: odfDisk, MasterCPUs: masterCPUs`; flags next to `--master-disk`:

```go
	cmd.Flags().IntVar(&masterCPUs, "master-cpus", 4, "Master node vCPUs (raised to 8 automatically with --odf)")
	cmd.Flags().BoolVar(&odf, "odf", false,
		"Install OpenShift Data Foundation after the cluster is up: a dedicated data disk, "+
			"LVMS thin pool, and a single-node Ceph StorageCluster (RBD + CephFS StorageClasses). "+
			"Raises the master to 8 vCPUs / 19456 MiB RAM. Needs the default OperatorHub catalogs (online).")
	cmd.Flags().IntVar(&odfDisk, "odf-disk", 0,
		"ODF backing data disk size in GB (sparse; default 100; usable Ceph capacity is about a third). Requires --odf.")
```

Note: `MasterCPUs` currently comes from a hardcoded literal in the create command — check and replace with the flag variable.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./app/ -run TestValidateODFDefaults` — PASS. Then `make check` — PASS.

- [ ] **Step 5: Commit**

```bash
git add -u && git commit -s -m "feat: --odf/--odf-disk/--master-cpus config and validation

Assisted-by: Claude Code/claude-fable-5"
```

---

### Task 2: `VMManager.CreateDataDisk`

**Files:**
- Modify: `interfaces/interfaces.go` (VMManager)
- Modify: `providers/libvirt/libvirt.go`
- Modify: `providers/vfkit/vfkit.go`
- Modify: `providers/fakes/fakes.go`
- Test: `providers/vfkit/vfkit_test.go`

**Interfaces:**
- Produces: `CreateDataDisk(ctx context.Context, pool, volName string, sizeGiB int) (string, error)` on `interfaces.VMManager` — creates an empty sparse writable disk, returns the attachable path. Idempotent: an existing volume/file of that name is reused.

- [ ] **Step 1: Write the failing test** (append to `providers/vfkit/vfkit_test.go`)

```go
func TestCreateDataDiskAndDeleteCleanup(t *testing.T) {
	m := newMgr(t)
	vol := config.ODFVolName("master-0-demo")
	got, err := m.CreateDataDisk(context.Background(), "ignored-pool", vol, 2)
	if err != nil {
		t.Fatalf("CreateDataDisk: %v", err)
	}
	fi, err := os.Stat(got)
	if err != nil || fi.Size() != 2<<30 {
		t.Fatalf("data disk wrong: %v size=%d", err, fi.Size())
	}
	// Idempotent: second call reuses, does not truncate away content.
	if again, err := m.CreateDataDisk(context.Background(), "p", vol, 2); err != nil || again != got {
		t.Fatalf("CreateDataDisk not idempotent: %q vs %q (%v)", again, got, err)
	}
	if err := m.Delete(context.Background(), "master-0-demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("data disk %q survived Delete", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./providers/vfkit/ -run TestCreateDataDisk` — FAIL (method undefined).

- [ ] **Step 3: Implement**

`interfaces/interfaces.go`, in `VMManager` after `ImportDisk`:

```go
	// CreateDataDisk creates an empty, sparse, writable per-cluster disk
	// (the ODF backing device) named volName and returns its attachable
	// path: a pool volume path on libvirt, a state-dir file on vfkit.
	// Idempotent — an existing volume of that name is reused.
	CreateDataDisk(ctx context.Context, pool, volName string, sizeGiB int) (string, error)
```

`providers/vfkit/vfkit.go` (next to ImportDisk; Delete already removes by name pattern — extend it):

```go
// CreateDataDisk creates (or reuses) a sparse raw disk in the state dir.
func (m *VMManager) CreateDataDisk(_ context.Context, _, volName string, sizeGiB int) (string, error) {
	dst := filepath.Join(m.stateDir, volName)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("vfkit: create data disk: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(int64(sizeGiB) << 30); err != nil {
		return "", fmt.Errorf("vfkit: size data disk: %w", err)
	}
	return dst, nil
}
```

Extend vfkit `Delete` to also remove the ODF disk (same pattern as the image store clone):

```go
	_ = os.Remove(filepath.Join(m.stateDir, config.ODFVolName(name)))
```

`providers/libvirt/libvirt.go` (mirror ImportDisk's structure — check how ImportDisk shells out; qcow2 via `virsh vol-create-as`):

```go
// CreateDataDisk creates (or reuses) a sparse qcow2 volume in the pool.
func (m *VMManager) CreateDataDisk(ctx context.Context, pool, volName string, sizeGiB int) (string, error) {
	// Reuse if present (resume/idempotency).
	if out, err := m.cmd.Run(ctx, "virsh", "vol-path", "--pool", pool, volName); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := m.cmd.Run(ctx, "virsh", "vol-create-as", pool, volName,
		fmt.Sprintf("%dG", sizeGiB), "--format", "qcow2"); err != nil {
		return "", fmt.Errorf("create data disk volume: %w", err)
	}
	out, err := m.cmd.Run(ctx, "virsh", "vol-path", "--pool", pool, volName)
	if err != nil {
		return "", fmt.Errorf("resolve data disk path: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
```

`providers/fakes/fakes.go` `VMManager`: add `CreatedDataDisks []string`, method records volName and returns `"/var/lib/libvirt/images/" + volName` (mirror ImportDisk's fake).

- [ ] **Step 4: Run tests**

Run: `go test ./providers/... ./app/...` — PASS. `make check` — PASS.

- [ ] **Step 5: Commit**

```bash
git add -u && git commit -s -m "feat: VMManager.CreateDataDisk on both backends

Assisted-by: Claude Code/claude-fable-5"
```

---

### Task 3: Attach the ODF data disk in create-master-vms

**Files:**
- Modify: `stages/createmastervms/stage.go`
- Test: `app/cluster_test.go`

**Interfaces:**
- Consumes: `CreateDataDisk` (Task 2), `config.ODFVolName`.
- Produces: on `--odf`, the master VM carries a writable, non-shareable ExtraDisk AFTER the bake store (if any).

- [ ] **Step 1: Write the failing test** (append to `app/cluster_test.go`)

```go
// TestCreateCluster_ODFDataDisk asserts the master gets a writable data disk
// after the bake store, and plain clusters get none.
func TestCreateCluster_ODFDataDisk(t *testing.T) {
	cfg, deps, bundle := newTestEnv(t)
	mgr := app.NewClusterManager(cfg, deps)

	c := newTestCluster("odfy")
	c.ODF = true
	c.BakeImages = true
	if err := mgr.Create(context.Background(), c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	disks := bundle.VM.Created[0].ExtraDisks
	if len(disks) != 2 {
		t.Fatalf("extra disks: got %d want 2 (store + odf)", len(disks))
	}
	odf := disks[1] // MUST be after the bake store: device path math depends on it
	if odf.ReadOnly || odf.Shareable || !strings.Contains(odf.Path, "-odf.") {
		t.Errorf("odf disk wrong: %+v", odf)
	}
	if len(bundle.VM.CreatedDataDisks) != 1 {
		t.Errorf("CreateDataDisk calls: got %d want 1", len(bundle.VM.CreatedDataDisks))
	}
}
```

- [ ] **Step 2: Run to verify FAIL**, then **Step 3: Implement** in `createMasterVM`, directly after the bake-images block that appends to `extraDisks`:

```go
	if c.ODF {
		path, err := s.vm.CreateDataDisk(ctx, c.StoragePool, config.ODFVolName(vmName), c.ODFDiskGB)
		if err != nil {
			return fmt.Errorf("create odf data disk: %w", err)
		}
		// Writable and exclusive; attached after the bake store so the guest
		// device path is deterministic (/dev/vdb, or /dev/vdc with baking).
		extraDisks = append(extraDisks, interfaces.ExtraDisk{Path: path})
	}
```

Also extend the darwin disk-space preflight comment: the ODF disk is sparse on both backends; do NOT add it to `need` (same clonefile/sparse argument as the bake store on darwin; on Linux `vol-create-as` qcow2 is also sparse).

- [ ] **Step 4: `make check`** — PASS. **Step 5: Commit** (`feat: attach the ODF data disk to the master VM`).

---

### Task 4: `interfaces.ODFInstaller` + fake

**Files:**
- Modify: `interfaces/interfaces.go`, `interfaces/deps.go`
- Modify: `providers/fakes/fakes.go`
- Test: compile-only (used by Tasks 5–7)

**Interfaces:**
- Produces:

```go
// ODFSpec carries everything the ODF installer needs for one cluster.
type ODFSpec struct {
	KubeconfigPath string
	OCBinaryPath   string
	WorkDir        string // cluster dir; rendered manifests are written under <WorkDir>/odf/
	Channel        string // e.g. "stable-4.22"
	DevicePath     string // /dev/vdb, or /dev/vdc with --bake-images
	DataPVCSizeGi  int    // per-OSD PVC request
}

// ODFInstaller drives the four ODF phases. Each call is idempotent and
// blocks until its phase is complete (or its internal timeout fires).
type ODFInstaller interface {
	InstallOperators(ctx context.Context, spec ODFSpec) error   // LVMS + ODF subs; waits CSVs + StorageCluster CRD
	SetupLVM(ctx context.Context, spec ODFSpec) error           // LVMCluster (waits Ready) + Immediate StorageClass
	EnableSingleNode(ctx context.Context, spec ODFSpec) error   // SINGLE_NODE patch + settle, node label, Driver CRs, monitoring trim
	CreateStorageCluster(ctx context.Context, spec ODFSpec) error // StorageCluster; waits Ready + both ceph StorageClasses
}
```

- [ ] **Step 1:** Add the types above to `interfaces/interfaces.go`; add `ODF ODFInstaller` to `Deps` in `interfaces/deps.go`.
- [ ] **Step 2:** Fake in `providers/fakes/fakes.go` (pattern: fake Installer): struct `ODFInstaller` with `mu sync.Mutex`, `Sequence []string`, `LastSpec interfaces.ODFSpec`, `Err error`; each method appends its name to `Sequence`, records spec, returns `Err`. Wire `ODF: &ODFInstaller{}` into `fakes.All()`'s bundle + Deps. Add a trace section in `WriteTrace` ("ODF phases invoked:" + sequence) when non-empty.
- [ ] **Step 3:** `make check` — PASS. Commit (`feat: ODFInstaller interface + fake`).

---

### Task 5: `providers/odf` renderers

**Files:**
- Create: `providers/odf/render.go`
- Test: `providers/odf/render_test.go`

**Interfaces:**
- Produces (all pure): `config.OLMChannelForVersion(v string) string` (lives in `config/paths.go` so the stage — which cannot import providers — uses it too), `RenderOperators(channel string) string`, `RenderLVMCluster(devicePath string) string`, `RenderImmediateStorageClass() string`, `RenderDriver(name string) string`, `RenderMonitoringTrim() string`, `RenderStorageCluster(pvcGi int) string`, `SingleNodePatch() string`. Constants: `ImmediateStorageClassName = "easyshift-odf-immediate"`, `RBDStorageClass = "ocs-storagecluster-ceph-rbd"`, `CephFSStorageClass = "ocs-storagecluster-cephfs"`.

- [ ] **Step 1: Write the failing tests** (`providers/odf/render_test.go`, package `odf`)

```go
package odf

import (
	"strings"
	"testing"
)

func TestChannelForVersion(t *testing.T) {
	// Implemented in config (stages cannot import providers); tested here
	// beside its main consumer.
	if got := config.OLMChannelForVersion("4.22.9"); got != "stable-4.22" {
		t.Errorf("channel: %q", got)
	}
}

func TestRenderStorageClusterSingleNodeDeltas(t *testing.T) {
	mc := RenderStorageCluster(30)
	// The three hardware-validated 4.22 deltas from the spec's spike results:
	for _, want := range []string{
		"count: 3", "replica: 1", // replica: 3 re-raises minNodes past SINGLE_NODE
		"preparePlacement:",                       // empty/absent placements render an invalid TSC
		"topologyKey: kubernetes.io/hostname",     // the no-op TSC
		"whenUnsatisfiable: ScheduleAnyway",
		"storage: 30Gi",
		"storageClassName: " + ImmediateStorageClassName,
		"reconcileStrategy: ignore",
		"volumeMode: Block",
	} {
		if !strings.Contains(mc, want) {
			t.Errorf("StorageCluster missing %q", want)
		}
	}
	if strings.Contains(mc, "replica: 3") {
		t.Error("replica: 3 defeats SINGLE_NODE's node-count relaxation on ODF 4.22")
	}
}

func TestRenderDriverFloorsAllContainers(t *testing.T) {
	d := RenderDriver("openshift-storage.rbd.csi.ceph.com")
	for _, want := range []string{"replicas: 1", "plugin:", "omapGenerator:", "addons:", "attacher:", "provisioner:", "resizer:", "snapshotter:"} {
		if !strings.Contains(d, want) {
			t.Errorf("Driver missing %q (default plugin alone is 250Mi)", want)
		}
	}
}

func TestRenderImmediateStorageClass(t *testing.T) {
	sc := RenderImmediateStorageClass()
	if !strings.Contains(sc, "volumeBindingMode: Immediate") || !strings.Contains(sc, "topolvm.io") {
		t.Errorf("SC not immediate/topolvm:\n%s", sc)
	}
}

func TestRenderOperatorsAndLVM(t *testing.T) {
	ops := RenderOperators("stable-4.22")
	for _, want := range []string{"openshift-lvm-storage", "openshift-storage", "lvms-operator", "odf-operator", "channel: stable-4.22", "redhat-operators"} {
		if !strings.Contains(ops, want) {
			t.Errorf("operators missing %q", want)
		}
	}
	lvm := RenderLVMCluster("/dev/vdc")
	if !strings.Contains(lvm, "/dev/vdc") || !strings.Contains(lvm, "thinPoolConfig") {
		t.Errorf("LVMCluster wrong:\n%s", lvm)
	}
}
```

- [ ] **Step 2: Run to verify FAIL** (package doesn't exist).
- [ ] **Step 3: Implement `providers/odf/render.go`.** Every manifest is exactly what the spike applied (copy from the spec's spike-validated YAML — operators trio ×2, LVMCluster with `deviceClasses[0] = {name: odf, default: true, fstype: xfs, deviceSelector.paths: [<devicePath>], thinPoolConfig: {name: thin, overprovisionRatio: 10, sizePercent: 90}}`, the Immediate SC, both Driver CRs with the full container floors, the monitoring-trim ConfigMap, and the StorageCluster with `count: 3, replica: 1`, no-op TSC in `placement` + `preparePlacement`, floored component resources, `monPVCTemplate` 2Gi, ignores for monitoring/objectstore/noobaa). `config.OLMChannelForVersion` (add to `config/paths.go`) splits on `.` and joins the first two as `stable-<maj>.<min>`. `SingleNodePatch()` returns `{"spec":{"config":{"env":[{"name":"SINGLE_NODE","value":"true"}]}}}`.
- [ ] **Step 4: `make check`** — PASS. **Step 5: Commit** (`feat: providers/odf manifest renderers`).

---

### Task 6: `providers/odf` installer

**Files:**
- Create: `providers/odf/installer.go`
- Test: `providers/odf/installer_test.go`

**Interfaces:**
- Consumes: renderers (Task 5), `interfaces.CommandRunner`.
- Produces: `odf.New(cmd interfaces.CommandRunner) *Installer` implementing `interfaces.ODFInstaller`.

Implementation notes (all four methods share helpers):

- `apply(ctx, spec, filename, content)`: write `content` to `<spec.WorkDir>/odf/<filename>` (0644, MkdirAll), then `cmd.Run(ctx, spec.OCBinaryPath, "--kubeconfig", spec.KubeconfigPath, "apply", "-f", <path>)`. (CommandRunner has no stdin — files are the transport, and they double as debugging artifacts.)
- `pollUntil(ctx, timeout, desc, probe func() (bool, error))`: loop every 10 s until probe true or timeout; on timeout return an error naming `desc`. Transient probe errors (API flap) are retried, not fatal.
- `ocJSONPath(ctx, spec, jsonpath string, args ...string) (string, error)`: wraps `cmd.Run` with `-o jsonpath=...`.
- Timeouts (constants, overridable in tests): `waitCSV = 15 * time.Minute`, `waitLVM = 5 * time.Minute`, `waitSettle = 5 * time.Minute`, `waitStorageCluster = 30 * time.Minute`, `waitStorageClasses = 10 * time.Minute`, poll 10 s.

Method behavior:

1. `InstallOperators`: apply `RenderOperators(spec.Channel)`; poll until the `lvms-operator` CSV in `openshift-lvm-storage` AND a CSV named `ocs-operator*` in `openshift-storage` are `Succeeded` AND `oc get crd storageclusters.ocs.openshift.io` succeeds.
2. `SetupLVM`: apply `RenderLVMCluster(spec.DevicePath)`; poll `lvmcluster easyshift-odf` `.status.state == "Ready"`; apply `RenderImmediateStorageClass()`.
3. `EnableSingleNode`: find the sub name via `oc get sub -n openshift-storage -o jsonpath={range .items[?(@.spec.name=="ocs-operator")]}{.metadata.name}{end}`; `oc patch sub ... --type merge -p SingleNodePatch()`; poll the `ocs-operator` deployment for the env value AND `Available=True`; `oc label nodes --all cluster.ocs.openshift.io/openshift-storage= --overwrite`; apply both `RenderDriver(...)` CRs and `RenderMonitoringTrim()`.
4. `CreateStorageCluster`: apply `RenderStorageCluster(spec.DataPVCSizeGi)`; poll StorageCluster `.status.phase == "Ready"`; poll until both `RBDStorageClass` and `CephFSStorageClass` exist.

- [ ] **Step 1: Write the failing test.** Use `fakes.CommandRunner` with a `RunFunc` that returns canned jsonpath outputs keyed on args (e.g. any args containing `"csv"` → `Succeeded`, `"lvmcluster"` → `Ready`, `"storagecluster"` → `Ready`, `"sub"` + jsonpath → the long metadata name, `"get sc"`-shape → both SC names) so each method completes in one poll. Assert: (a) the calls hit `spec.OCBinaryPath` with `--kubeconfig spec.KubeconfigPath`; (b) rendered files exist under `WorkDir/odf/`; (c) `EnableSingleNode` patches the sub by the name returned from the `spec.name` query; (d) a probe that never converges returns a timeout error naming the phase (override the timeout constant to ~1 s in the test).
- [ ] **Step 2: FAIL** (package half-missing) → **Step 3: implement** → **Step 4: `make check` PASS** → **Step 5: commit** (`feat: providers/odf installer`).

---

### Task 7: `install-odf` stage + wiring

**Files:**
- Create: `stages/installodf/stage.go`
- Modify: `app/manager.go` (buildStages), `app/deps.go` (NewProductionDeps)
- Test: `stages/installodf/stage_test.go`, `app/cluster_test.go`

**Interfaces:**
- Consumes: `interfaces.ODFInstaller` (Task 4), `ODFVolName`/device-order rule (Task 3), `config.OLMChannelForVersion` (defined in Task 5 in `config/paths.go`, because stages cannot import providers).
- Produces: stage `install-odf` in the list after `merge-kubeconfig`, before `finalize`.

Stage shape (mirror `bakeimagestore`):

```go
// Package installodf installs OpenShift Data Foundation on the converged
// cluster: operators, LVMS thin pool on the dedicated data disk, the
// single-node enablement tricks, and the trimmed StorageCluster. No-op
// unless the cluster opted in with --odf.
package installodf

type Stage struct{ odf interfaces.ODFInstaller }

func New(odf interfaces.ODFInstaller) *Stage { return &Stage{odf: odf} }

func (*Stage) Name() string { return "install-odf" }

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	if !sc.Cluster.ODF {
		return nil
	}
	spec := s.spec(sc)
	if err := s.odf.InstallOperators(ctx, spec); err != nil {
		return fmt.Errorf("odf operators: %w", err)
	}
	if err := s.odf.SetupLVM(ctx, spec); err != nil {
		return fmt.Errorf("odf lvm: %w", err)
	}
	if err := s.odf.EnableSingleNode(ctx, spec); err != nil {
		return fmt.Errorf("odf single-node enablement: %w", err)
	}
	if err := s.odf.CreateStorageCluster(ctx, spec); err != nil {
		return fmt.Errorf("odf storagecluster: %w", err)
	}
	return nil
}

// Rollback is a no-op: it only runs on cluster delete, where the VM and both
// disks are destroyed moments later; a graceful in-cluster teardown would
// gain nothing and could wedge delete behind Ceph finalizers on a broken
// cluster. A future day-2 "odf remove" command would own the ordered
// teardown (StorageCluster -> operators -> VG/disk).
func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

func (s *Stage) spec(sc *interfaces.StageContext) interfaces.ODFSpec {
	device := "/dev/vdb"
	if sc.Cluster.BakeImages {
		device = "/dev/vdc" // the bake store occupies vdb
	}
	return interfaces.ODFSpec{
		KubeconfigPath: filepath.Join(sc.ClusterDir(), "auth", "kubeconfig"),
		OCBinaryPath:   sc.OCBinaryPath(),
		WorkDir:        sc.ClusterDir(),
		Channel:        config.OLMChannelForVersion(sc.Cluster.OCPVersion),
		DevicePath:     device,
		DataPVCSizeGi:  sc.Cluster.ODFDiskGB/3 - 3,
	}
}
```

- [ ] **Step 1: Failing stage test** (`stages/installodf/stage_test.go`): with a `fakes.ODFInstaller`, a cluster with `ODF: true, BakeImages: true, ODFDiskGB: 100, OCPVersion: "4.22.9"`, assert `Sequence == [InstallOperators SetupLVM EnableSingleNode CreateStorageCluster]`, `LastSpec.DevicePath == "/dev/vdc"`, `LastSpec.Channel == "stable-4.22"`, `LastSpec.DataPVCSizeGi == 30`; and that `ODF: false` produces an empty sequence. Also an app-level assertion in `TestCreateCluster_ODFDataDisk` (Task 3's test): `bundle.ODF.Sequence` non-empty.
- [ ] **Step 2: FAIL** → **Step 3: implement stage + wire**: `app/manager.go` buildStages inserts `installodf.New(d.ODF)` after `mergekubeconfig`, before `finalize`; `app/deps.go` `NewProductionDeps` sets `ODF: odf.New(cmd)`.
- [ ] **Step 4:** `make check` PASS; `./easyshift create -n demo --odf --simulate` shows the four ODF phases in the trace.
- [ ] **Step 5: Commit** (`feat: install-odf stage wired into the pipeline`).

---

### Task 8: Docs + ROADMAP

**Files:**
- Create: `docs/dev/odf.md` (design summary + the 4.22 recipe deltas + resource floors; link the spec and credit the dfmicro recipe)
- Modify: `docs/README.md` (index row), `docs/user/*` usage doc (`--odf` flag, capacity math, 24 GB-host caveat, online-catalogs requirement), `ROADMAP.md` (add: day-2 `odf remove`; note --odf conflicts with future offline mode)
- [ ] **Step 1:** Write the docs. **Step 2:** `make check` (lint includes docs-adjacent checks) PASS. **Step 3:** Commit (`docs: --odf usage + internals`).

---

### Task 9: Hardware validation

- [ ] **Step 1:** Free resources: `easyshift delete baked` (it currently holds hand-edited spec + spike leftovers; ~200 GB of sparse files). Keep the imagestore cache.
- [ ] **Step 2:** `./easyshift create -n odfy --odf --master-disk 60` (no bake: also validates the `/dev/vdb` path). Expect ~25 min to cluster + ~20-30 min to StorageCluster Ready.
- [ ] **Step 3:** Acceptance: `oc get storagecluster -n openshift-storage` Ready; `oc get cephcluster` HEALTH_OK; both SCs exist; a 1Gi PVC on `ocs-storagecluster-ceph-rbd` + pod writes a file (the spike's `odf-write-test` manifest); `easyshift stop/start odfy` and re-check Ceph health (validates device-path stability across reboots).
- [ ] **Step 4:** Record results in the spec (new "Implementation validation" section) and tick this plan. Commit.

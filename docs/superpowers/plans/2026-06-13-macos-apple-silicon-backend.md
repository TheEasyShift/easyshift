# macOS (Apple Silicon) Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a macOS/Apple-Silicon provider backend (vfkit compute + vmnet-helper networking) behind the existing interfaces, wired by `runtime.GOOS`, so the install pipeline compiles, unit-tests, and runs under `--simulate` for darwin.

**Architecture:** New `providers/vfkit` (`VMManager`) and `providers/vmnethelper` (`NetworkProvisioner`) shell out to `vfkit` / `vmnet-helper` exactly as `providers/libvirt` shells out to `virsh` / `virt-install`. `app/deps.go` gains `NewDarwinDeps` and dispatches on `runtime.GOOS`. Architecture (x86_64 vs aarch64) is parameterized in `providers/openshift`. The libvirt-named network stage is renamed to a backend-neutral one.

**Tech Stack:** Go 1.24, cobra CLI, the project's interface/provider/stage/app layering, the `providers/fakes` test doubles, `make test` (`go test ./...`).

**Environment:** the dev machine is the target hardware — macOS 26.5.1 (Tahoe), Apple Silicon — with `vfkit 0.6.3` (on `PATH`) and `vmnet-helper 0.12.0` (Homebrew, at `/opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper`) installed. `make test`, the `go build`, and the on-hardware boot all run here natively (arm64); the Linux path is the cross-compile here. CI is `ubuntu-latest`.

**Scope of THIS plan:** two phases. **Phase A (Tasks 1–9):** the provider/wiring/refactor foundation — compiles, passes `make test`, cross-builds, and runs the macOS pipeline under `--simulate`. All provider command/arg construction is unit-tested against the fake `CommandRunner`. **Phase B (Tasks 10–13):** on-hardware validation executed on this machine — install the vmnet-helper privilege, boot a real aarch64 SNO, confirm Rosetta in-guest, and run the two-cluster DR check. The design doc is `docs/superpowers/specs/2026-06-13-macos-apple-silicon-support-design.md`.

**Networking model correction (drives Task 4):** `vmnet-helper` is **not** a standalone network daemon. Its CLI *requires* `--fd FD | --socket SOCKET` and is bound to exactly one VM — it is a per-VM privileged sidecar. So there is no one-time "create the shared network" shell-out; the shared `192.168.126.0/24` network materializes when each VM's sidecar starts in `--operation-mode shared` with the same subnet. VM start and network attach are therefore coupled inside `providers/vfkit` + `providers/vmnethelper` (see the spec's "VM lifecycle coupling"). Phase A unit-tests the sidecar argv builder; Phase B wires the live `sudo` spawn + fd handshake.

**Conventions for every commit in this plan:** use `git commit -s` and end the message with `Assisted-by: Claude Code/claude-opus-4-8` (per repo CLAUDE.md). Run `make test` (gofmt + vet + `go test ./...`) before each commit; it must pass.

---

## File Structure

| File | Responsibility |
|---|---|
| `providers/openshift/arch.go` (create) | Pure arch helpers: payload arch key, host client-tarball platform, arch-aware mirror/client URLs. |
| `providers/openshift/arch_test.go` (create) | Table tests for the arch helpers. |
| `providers/openshift/installer.go` (modify) | `CoreOSLiveISOURL` takes the target arch instead of the `coreOSArch` const. |
| `providers/openshift/rhcos.go` (modify) | `OCPClientURL`/`ReleaseTxtURL` become arch-aware. |
| `stages/downloadbinaries/stage.go` (modify) | Build client URLs from host platform + payload arch. |
| `providers/host/host.go` (modify) | Keep only cross-platform methods; OS-specific ones move out. |
| `providers/host/host_linux.go` (create) | Linux `HasCPUVirtualization`/`InspectBridge`/`ARPLookup`. |
| `providers/host/host_darwin.go` (create) | Darwin equivalents (Apple Silicon CPU virt, stubs for the rest). |
| `providers/vfkit/vfkit.go` (create) | `VMManager` over `vfkit` (process supervisor). |
| `providers/vfkit/vfkit_test.go` (create) | vfkit arg-construction + supervision unit tests. |
| `providers/vmnethelper/vmnethelper.go` (create) | `NetworkProvisioner` over `vmnet-helper`. |
| `providers/vmnethelper/vmnethelper_test.go` (create) | vmnet-helper arg-construction unit tests. |
| `stages/createnetwork/` (rename from `createlibvirtnetwork/`) | Backend-neutral "attach cluster to shared NAT network" stage. |
| `stages/publishpxeassets/stage.go` (create) | macOS boot-media stage (PXE assets to the fileserver) replacing `embed-ignition-iso`. |
| `providers/openshift/rosetta.go` (create) | Build the Rosetta guest MachineConfig/ignition fragment. |
| `app/deps.go` (modify) | `NewDarwinDeps` + `runtime.GOOS` dispatch. |
| `app/manager.go` (modify) | Rename import; per-OS boot-media stage selection. |
| `app/status.go` (modify) | `checkVMState` via `VMManager.IsRunning` instead of raw `virsh`. |

---

## Task 1: Architecture parameterization in `providers/openshift`

**Files:**
- Create: `providers/openshift/arch.go`
- Create: `providers/openshift/arch_test.go`
- Modify: `providers/openshift/rhcos.go:14-17` (`OCPClientURL`), `:44-49` (`ReleaseTxtURL`)
- Modify: `providers/openshift/installer.go:32` (drop the `coreOSArch` const), `:185-191` (`CoreOSLiveISOURL`)
- Modify: `stages/downloadbinaries/stage.go:40-49`

Context: `config.OCPMirrorURL` is hard-pinned to `https://mirror.openshift.com/pub/openshift-v4/x86_64`, `OCPClientURL` always builds `…-linux.tar.gz`, and `CoreOSLiveISOURL` passes the `coreOSArch = "x86_64"` const. An arm64 cluster on a mac needs payload arch `aarch64`, mirror `…/aarch64`, and host client tarballs `openshift-install-mac-arm64.tar.gz` / `openshift-client-mac-arm64.tar.gz`.

- [ ] **Step 1: Write the failing test**

Create `providers/openshift/arch_test.go`:

```go
package openshift_test

import (
	"testing"

	"github.com/TheEasyShift/easyshift/providers/openshift"
)

func TestPayloadArch(t *testing.T) {
	cases := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}
	for goarch, want := range cases {
		if got := openshift.PayloadArch(goarch); got != want {
			t.Errorf("PayloadArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestHostClientPlatform(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "mac-arm64"},
		{"darwin", "amd64", "mac"},
		{"linux", "amd64", "linux"},
		{"linux", "arm64", "linux"},
	}
	for _, c := range cases {
		if got := openshift.HostClientPlatform(c.goos, c.goarch); got != c.want {
			t.Errorf("HostClientPlatform(%q,%q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestOCPClientURL_ArchAware(t *testing.T) {
	got := openshift.OCPClientURL("aarch64", "4.21.0", "openshift-install-mac-arm64.tar.gz")
	want := "https://mirror.openshift.com/pub/openshift-v4/aarch64/clients/ocp/4.21.0/openshift-install-mac-arm64.tar.gz"
	if got != want {
		t.Errorf("OCPClientURL = %q, want %q", got, want)
	}
}

func TestInstallClientTarball(t *testing.T) {
	if got := openshift.InstallClientTarball("mac-arm64"); got != "openshift-install-mac-arm64.tar.gz" {
		t.Errorf("InstallClientTarball = %q", got)
	}
	if got := openshift.OCClientTarball("linux"); got != "openshift-client-linux.tar.gz" {
		t.Errorf("OCClientTarball = %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./providers/openshift/ -run 'TestPayloadArch|TestHostClientPlatform|TestOCPClientURL_ArchAware|TestInstallClientTarball' -v`
Expected: FAIL — `undefined: openshift.PayloadArch` (and the other new symbols).

- [ ] **Step 3: Write minimal implementation**

Create `providers/openshift/arch.go`:

```go
package openshift

import "fmt"

// PayloadArch maps a Go arch (runtime.GOARCH) to the OpenShift mirror /
// RHCOS stream-json architecture key. easyshift runs a native-arch guest, so
// the cluster payload arch equals the host CPU arch.
func PayloadArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// HostClientPlatform returns the openshift-install/oc tarball platform suffix
// for the host easyshift runs on (where those binaries execute).
func HostClientPlatform(goos, goarch string) string {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "mac-arm64"
	case goos == "darwin":
		return "mac"
	default:
		return "linux"
	}
}

// InstallClientTarball / OCClientTarball name the client tarballs for a host
// platform suffix (from HostClientPlatform).
func InstallClientTarball(platform string) string {
	return fmt.Sprintf("openshift-install-%s.tar.gz", platform)
}

func OCClientTarball(platform string) string {
	return fmt.Sprintf("openshift-client-%s.tar.gz", platform)
}

// OCPMirrorURLForArch is the mirror root for a payload architecture.
func OCPMirrorURLForArch(arch string) string {
	return fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/%s", arch)
}
```

Then make `OCPClientURL` and `ReleaseTxtURL` arch-aware. In `providers/openshift/rhcos.go`, replace lines 14-17:

```go
// OCPClientURL constructs the mirror URL for openshift-install / openshift-client
// for a given payload architecture.
func OCPClientURL(arch, version, tarball string) string {
	return fmt.Sprintf("%s/clients/ocp/%s/%s", OCPMirrorURLForArch(arch), version, tarball)
}
```

and replace `ReleaseTxtURL` (lines 44-49):

```go
// ReleaseTxtURL is the URL to fetch the channel's release.txt index for a
// payload architecture.
func ReleaseTxtURL(arch, channel string) string {
	return fmt.Sprintf("%s/clients/ocp/%s/release.txt", OCPMirrorURLForArch(arch), channel)
}
```

Update `ResolveOCPVersion` (rhcos.go:54) to thread arch through to `ReleaseTxtURL`:

```go
func ResolveOCPVersion(ctx context.Context, dl interfaces.Downloader, arch, channel string) (string, error) {
```
and at its `ReleaseTxtURL(channel)` call site change it to `ReleaseTxtURL(arch, channel)`.

In `installer.go`, delete the `coreOSArch` const (line 32) and change `CoreOSLiveISOURL` to take the arch from the spec's cluster host. Since `InstallerSpec` does not carry arch, add an `Arch` field. In `interfaces/interfaces.go` `InstallerSpec` (after `CoreOSInstallerPath`), add:

```go
	// Arch is the payload architecture key ("x86_64" / "aarch64") for mirror
	// + stream-json lookups. Set by the wiring from runtime.GOARCH.
	Arch string
```

Then in `installer.go` `CoreOSLiveISOURL`, change the final line from `return parseCoreOSLiveISO(stdout.Bytes(), coreOSArch)` to:

```go
	arch := spec.Arch
	if arch == "" {
		arch = "x86_64"
	}
	return parseCoreOSLiveISO(stdout.Bytes(), arch)
```

- [ ] **Step 4: Update the call sites that now fail to compile**

In `stages/downloadbinaries/stage.go`, replace lines 40-49 with host-platform-aware URLs (add `runtime` to imports):

```go
	plat := openshift.HostClientPlatform(runtime.GOOS, runtime.GOARCH)
	arch := openshift.PayloadArch(runtime.GOARCH)
	if _, err := os.Stat(filepath.Join(binDir, "openshift-install")); err != nil {
		if err := s.downloadTarball(ctx, openshift.OCPClientURL(arch, sc.Cluster.OCPVersion, openshift.InstallClientTarball(plat)), binDir); err != nil {
			return fmt.Errorf("download openshift-install: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(binDir, "oc")); err != nil {
		if err := s.downloadTarball(ctx, openshift.OCPClientURL(arch, sc.Cluster.OCPVersion, openshift.OCClientTarball(plat)), binDir); err != nil {
			return fmt.Errorf("download oc: %w", err)
		}
	}
```

Find any other callers of `OCPClientURL`, `ReleaseTxtURL`, `ResolveOCPVersion`, or `config.OCPMirrorURL` and update them: `grep -rn 'OCPClientURL\|ReleaseTxtURL\|ResolveOCPVersion\|OCPMirrorURL' --include='*.go' .`. For each, pass `openshift.PayloadArch(runtime.GOARCH)` as the arch (or `"x86_64"` in test fixtures that assert a literal URL — update those literals to the arch they pass).

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test`
Expected: PASS (all packages, including the new arch tests).

- [ ] **Step 6: Commit**

```bash
git add providers/openshift/arch.go providers/openshift/arch_test.go providers/openshift/rhcos.go providers/openshift/installer.go interfaces/interfaces.go stages/downloadbinaries/stage.go
git commit -s -m "openshift: parameterize architecture for mirror/client/stream URLs

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 2: Split `providers/host` by OS with build tags

**Files:**
- Modify: `providers/host/host.go`
- Create: `providers/host/host_linux.go`
- Create: `providers/host/host_darwin.go`
- Create: `providers/host/host_darwin_test.go`

Context: `HasCPUVirtualization` (`/proc/cpuinfo`), `InspectBridge` (`/sys/class/net`), and `ARPLookup` (`/proc/net/arp`) are Linux-only at runtime. Keep `LookPath`, `AvailableDiskBytes`, `DialTCP`, and the struct in `host.go` (they compile and behave correctly on both OSes via `golang.org/x/sys/unix`). Move the three Linux-only methods to `host_linux.go` and add darwin equivalents.

- [ ] **Step 1: Write the failing test**

Create `providers/host/host_darwin_test.go`:

```go
//go:build darwin

package host

import "testing"

func TestDarwin_HasCPUVirtualization_AppleSilicon(t *testing.T) {
	ok, err := SystemHostInspector{}.HasCPUVirtualization()
	if err != nil {
		t.Fatalf("HasCPUVirtualization: %v", err)
	}
	if !ok {
		t.Error("expected CPU virtualization available on this Apple Silicon host")
	}
}
```

- [ ] **Step 2: Run test to verify it fails (on darwin)**

Run (on a mac): `go test ./providers/host/ -run TestDarwin_HasCPUVirtualization_AppleSilicon -v`
Expected: FAIL to compile — `HasCPUVirtualization` still reads `/proc/cpuinfo` in the untagged `host.go`, producing a duplicate once `host_darwin.go` is added; until then the darwin behavior is wrong (returns an error reading `/proc/cpuinfo`).
(If implementing on Linux, this test is skipped by the build tag; verify the Linux build still passes after the move in Step 4.)

- [ ] **Step 3: Move the three Linux-only methods out of `host.go`**

In `host.go`, delete the bodies of `HasCPUVirtualization` (lines 24-36), `InspectBridge` (lines 38-64), and `ARPLookup` (lines 86-106), and drop now-unused imports (`bytes`, `strings`, `path/filepath` only if no longer used; keep what `AvailableDiskBytes`/`DialTCP` need). Create `providers/host/host_linux.go`:

```go
//go:build linux

package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheEasyShift/easyshift/interfaces"
)

func (SystemHostInspector) HasCPUVirtualization() (bool, error) {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false, fmt.Errorf("read /proc/cpuinfo: %w", err)
	}
	return bytes.Contains(data, []byte(" vmx ")) ||
		bytes.Contains(data, []byte("\tvmx ")) ||
		bytes.Contains(data, []byte(" svm ")) ||
		bytes.Contains(data, []byte("\tsvm ")), nil
}

func (SystemHostInspector) InspectBridge(name string) (interfaces.BridgeInfo, error) {
	base := filepath.Join("/sys/class/net", name)
	if _, err := os.Stat(filepath.Join(base, "bridge")); err != nil {
		if os.IsNotExist(err) {
			return interfaces.BridgeInfo{Exists: false}, nil
		}
		return interfaces.BridgeInfo{}, err
	}
	info := interfaces.BridgeInfo{Exists: true}
	entries, err := os.ReadDir(filepath.Join(base, "brif"))
	if err != nil && !os.IsNotExist(err) {
		return interfaces.BridgeInfo{}, fmt.Errorf("read brif for %s: %w", name, err)
	}
	for _, e := range entries {
		info.Slaves = append(info.Slaves, e.Name())
	}
	state, err := os.ReadFile(filepath.Join(base, "operstate"))
	if err != nil {
		return interfaces.BridgeInfo{}, fmt.Errorf("read operstate for %s: %w", name, err)
	}
	info.Up = strings.TrimSpace(string(state)) == "up"
	return info, nil
}

func (SystemHostInspector) ARPLookup(mac string) (string, error) {
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return "", fmt.Errorf("read /proc/net/arp: %w", err)
	}
	want := strings.ToLower(mac)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if strings.ToLower(fields[3]) == want {
			return fields[0], nil
		}
	}
	return "", nil
}
```

Create `providers/host/host_darwin.go`:

```go
//go:build darwin

package host

import (
	"os/exec"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// HasCPUVirtualization reports whether the host can run Virtualization.framework
// guests. Every Apple Silicon Mac on a supported macOS can, so we confirm the
// hardware feature via sysctl `hw.optional.arm64` (1 on Apple Silicon).
func (SystemHostInspector) HasCPUVirtualization() (bool, error) {
	out, err := exec.Command("sysctl", "-n", "hw.optional.arm64").Output()
	if err != nil {
		return false, nil // not Apple Silicon (Intel Mac) → no native arm64 virt
	}
	return len(out) > 0 && out[0] == '1', nil
}

// InspectBridge is not used on macOS in this phase (bridge mode is deferred);
// report "not a bridge" so NAT-mode preflight is unaffected.
func (SystemHostInspector) InspectBridge(_ string) (interfaces.BridgeInfo, error) {
	return interfaces.BridgeInfo{Exists: false}, nil
}

// ARPLookup shells out to `arp -n <ip>` is keyed by IP, not MAC; bridge-mode IP
// verification is deferred on macOS, so return "" (no entry) here.
func (SystemHostInspector) ARPLookup(_ string) (string, error) {
	return "", nil
}
```

- [ ] **Step 4: Run tests**

Run on Linux: `make test` → PASS (the Linux methods now live in `host_linux.go`).
Run on macOS (if available): `go test ./providers/host/ -v` → PASS including `TestDarwin_HasCPUVirtualization_AppleSilicon`.

- [ ] **Step 5: Verify cross-compilation**

Run: `GOOS=darwin GOARCH=arm64 go build ./... && GOOS=linux GOARCH=amd64 go build ./...`
Expected: both succeed.

- [ ] **Step 6: Commit**

```bash
git add providers/host/
git commit -s -m "host: split OS-specific inspector methods behind build tags

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 3: `providers/vfkit` — VMManager over vfkit (process supervisor)

**Files:**
- Create: `providers/vfkit/vfkit.go`
- Create: `providers/vfkit/vfkit_test.go`

Context: vfkit is one foreground process per running VM (no daemon). `Create` records a launch spec; `Start` spawns vfkit detached and records its pid; `IsRunning`/`Stop` use the saved pid; `Delete` removes spec + disk. `ImportISO`/`RemoveISO`/`StoragePoolActive` are libvirt-only and become no-ops. We unit-test the `vfkit` argument list (the verifiable part), mirroring `TestLibvirtVMManager_CreateArgs`. The VM launch spec is persisted under a per-VM dir so the supervisor survives across `easyshift` invocations.

This task constructs args from vfkit's documented flags (`--cpus`, `--memory`, `--device virtio-blk,path=…`, `--device virtio-net,fd=…`, `--device rosetta,mountTag=rosetta`, `--bootloader linux,kernel=…,initrd=…,cmdline=…`, `--restful-uri`). Exact flag spellings are confirmed against the real binary in the follow-up hardware plan; the test pins our intended contract.

- [ ] **Step 1: Write the failing test**

Create `providers/vfkit/vfkit_test.go`:

```go
package vfkit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/vfkit"
)

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func hasArgContaining(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func TestVFKit_StartArgs(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())

	if err := vm.Create(context.Background(), interfaces.VMSpec{
		Name:        "master-0-demo",
		MemoryMiB:   16000,
		VCPUs:       4,
		DiskSizeGiB: 120,
		MAC:         "52:54:00:11:22:33",
		NetworkArg:  "fd=3",
		KernelArgs:  "ignition.config.url=http://10.0.0.1:9393/demo/config.ign",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := vm.Start(context.Background(), "master-0-demo"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var start *fakes.CommandCall
	for i := range cmd.Calls {
		if cmd.Calls[i].Name == "vfkit" {
			start = &cmd.Calls[i]
		}
	}
	if start == nil {
		t.Fatal("expected a vfkit invocation")
	}
	joined := strings.Join(start.Args, " ")
	if !contains(start.Args, "--cpus") || !contains(start.Args, "4") {
		t.Errorf("missing --cpus 4: %s", joined)
	}
	if !contains(start.Args, "--memory") || !contains(start.Args, "16000") {
		t.Errorf("missing --memory 16000: %s", joined)
	}
	if !hasArgContaining(start.Args, "virtio-net") {
		t.Errorf("missing virtio-net device: %s", joined)
	}
	if !hasArgContaining(start.Args, "ignition.config.url=") {
		t.Errorf("kernel cmdline must carry ignition.config.url: %s", joined)
	}
	if !hasArgContaining(start.Args, "--restful-uri") {
		t.Errorf("missing --restful-uri for lifecycle control: %s", joined)
	}
}

func TestVFKit_IsRunning_FalseBeforeStart(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())
	if err := vm.Create(context.Background(), interfaces.VMSpec{Name: "m"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	running, err := vm.IsRunning(context.Background(), "m")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running {
		t.Error("VM should not be running before Start")
	}
}

func TestVFKit_ISONoops(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	vm := vfkit.NewVMManager(cmd, t.TempDir())
	if _, err := vm.ImportISO(context.Background(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("ImportISO should be a no-op on vfkit: %v", err)
	}
	if err := vm.StoragePoolActive(context.Background(), "p"); err != nil {
		t.Errorf("StoragePoolActive should be a no-op on vfkit: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./providers/vfkit/ -v`
Expected: FAIL — `package providers/vfkit is not in std` / `undefined: vfkit.NewVMManager`.

- [ ] **Step 3: Write minimal implementation**

Create `providers/vfkit/vfkit.go`:

```go
// Package vfkit implements interfaces.VMManager on macOS by shelling out to
// vfkit (Apple Virtualization.framework). Unlike libvirtd, vfkit is one
// process per running VM, so this manager is a process supervisor: it
// persists a launch spec per VM and tracks the running pid.
package vfkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// VMManager supervises vfkit processes. stateDir holds one subdir per VM with
// its launch spec and pid file.
type VMManager struct {
	cmd      interfaces.CommandRunner
	stateDir string
}

// NewVMManager returns a vfkit-backed VMManager. stateDir is a writable dir
// (e.g. <configDir>/vfkit) where per-VM specs and pid files live.
func NewVMManager(cmd interfaces.CommandRunner, stateDir string) *VMManager {
	return &VMManager{cmd: cmd, stateDir: stateDir}
}

type launchSpec struct {
	Spec    interfaces.VMSpec
	DiskPath string
}

func (m *VMManager) vmDir(name string) string  { return filepath.Join(m.stateDir, name) }
func (m *VMManager) specPath(name string) string { return filepath.Join(m.vmDir(name), "spec.json") }
func (m *VMManager) pidPath(name string) string  { return filepath.Join(m.vmDir(name), "vfkit.pid") }
func (m *VMManager) restPath(name string) string { return filepath.Join(m.vmDir(name), "rest.sock") }

// Create persists the launch spec and pre-allocates the disk image path. It
// does not start vfkit (Start does), mirroring libvirt's define/start split.
func (m *VMManager) Create(_ context.Context, spec interfaces.VMSpec) error {
	if err := os.MkdirAll(m.vmDir(spec.Name), 0o755); err != nil {
		return fmt.Errorf("vfkit: mkdir vm dir: %w", err)
	}
	ls := launchSpec{Spec: spec, DiskPath: filepath.Join(m.vmDir(spec.Name), "disk.img")}
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("vfkit: marshal spec: %w", err)
	}
	return os.WriteFile(m.specPath(spec.Name), data, 0o644)
}

// Start spawns vfkit detached and records its pid. The detached process keeps
// running across easyshift invocations; the watchdog in waitforinstall calls
// Start again if IsRunning reports the VM went away.
func (m *VMManager) Start(ctx context.Context, name string) error {
	ls, err := m.load(name)
	if err != nil {
		return err
	}
	args := m.buildArgs(name, ls)
	// The fake CommandRunner records this; the real spawn is handled by a
	// detached exec in production (see startDetached). For testability we run
	// through cmd.Run, which the production runner implements as a detached
	// start that writes the pid file.
	if _, err := m.cmd.Run(ctx, "vfkit", args...); err != nil {
		return fmt.Errorf("vfkit start %s: %w", name, err)
	}
	return nil
}

// buildArgs assembles the vfkit command line from the launch spec.
func (m *VMManager) buildArgs(name string, ls launchSpec) []string {
	s := ls.Spec
	args := []string{
		"--cpus", strconv.Itoa(s.VCPUs),
		"--memory", strconv.Itoa(s.MemoryMiB),
		"--device", "virtio-blk,path=" + ls.DiskPath,
		"--device", "virtio-net," + s.NetworkArg + ",mac=" + s.MAC,
		"--device", "rosetta,mountTag=rosetta",
		"--restful-uri", "unix://" + m.restPath(name),
	}
	if s.KernelArgs != "" {
		args = append(args, "--bootloader",
			"linux,kernel="+filepath.Join(m.vmDir(name), "vmlinuz")+
				",initrd="+filepath.Join(m.vmDir(name), "initrd.img")+
				",cmdline=\""+s.KernelArgs+"\"")
	}
	return args
}

// IsRunning reports whether the recorded pid is alive.
func (m *VMManager) IsRunning(_ context.Context, name string) (bool, error) {
	pid, err := m.readPID(name)
	if err != nil {
		return false, nil // no pid file → not started
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	return proc.Signal(syscall.Signal(0)) == nil, nil
}

// Stop signals the vfkit process to terminate.
func (m *VMManager) Stop(_ context.Context, name string) error {
	pid, err := m.readPID(name)
	if err != nil {
		return nil // nothing to stop
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)
	return nil
}

// Delete stops the VM and removes its state dir (spec, pid, disk).
func (m *VMManager) Delete(ctx context.Context, name string) error {
	_ = m.Stop(ctx, name)
	return os.RemoveAll(m.vmDir(name))
}

// CheckAccess verifies vfkit is usable. The presence check is done in
// preflight via HostInspector.LookPath; here we report success.
func (m *VMManager) CheckAccess(_ context.Context) error { return nil }

// ImportISO / RemoveISO / StoragePoolActive are libvirt storage-pool concepts
// with no vfkit analog (boot uses PXE assets served over HTTP). No-ops.
func (m *VMManager) ImportISO(_ context.Context, _, _, _ string) (string, error) { return "", nil }
func (m *VMManager) RemoveISO(_ context.Context, _, _ string) error              { return nil }
func (m *VMManager) StoragePoolActive(_ context.Context, _ string) error         { return nil }

func (m *VMManager) load(name string) (launchSpec, error) {
	var ls launchSpec
	data, err := os.ReadFile(m.specPath(name))
	if err != nil {
		return ls, fmt.Errorf("vfkit: read spec %s: %w", name, err)
	}
	return ls, json.Unmarshal(data, &ls)
}

func (m *VMManager) readPID(name string) (int, error) {
	data, err := os.ReadFile(m.pidPath(name))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}
```

Note for the executor: the test drives `Start` through `cmd.Run` so the args are recorded by the fake. In production, `NewExecCommandRunner` runs vfkit synchronously; a follow-up step (hardware plan) replaces the production `Start` path with a detached spawn that writes `pidPath`. Keep that production detail out of this CI-testable task.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./providers/vfkit/ -v`
Expected: PASS (`TestVFKit_StartArgs`, `TestVFKit_IsRunning_FalseBeforeStart`, `TestVFKit_ISONoops`).

- [ ] **Step 5: Commit**

```bash
git add providers/vfkit/
git commit -s -m "vfkit: add VMManager that supervises vfkit processes

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 4: `providers/vmnethelper` — NetworkProvisioner + per-VM sidecar seam

**Files:**
- Create: `providers/vmnethelper/vmnethelper.go`
- Create: `providers/vmnethelper/vmnethelper_test.go`

Context (corrected — see plan header "Networking model correction"): `vmnet-helper` is **not** a standalone daemon. It requires `--fd|--socket` and is bound to one VM, so there is no one-time network-create shell-out. The shared `192.168.126.0/24` network materializes when each VM's sidecar runs in shared mode with the same subnet. This task delivers three pure, unit-testable pieces and the stage-facing `NetworkProvisioner`:

1. `ResolveBinary()` — find the vmnet-helper binary at its known install locations (brew `libexec`, upstream `/opt/vmnet-helper/bin`), since it is **not** on `PATH` (decision 7).
2. `SidecarArgv(socketPath, subnet)` — build the per-VM `vmnet-helper` argument list (shared mode, our gateway/subnet). This is the contract the live `sudo` spawn (Phase B, Task 11) uses.
3. The `NetworkProvisioner` methods, which on macOS are bookkeeping/validation (no shell-out): `EnsureNetwork` validates the shared-network identity, `AddHost`/`RemoveHost` track `GlobalState` allocation (IPs are pinned via the ignition keyfile, decision 6), `InspectNetwork` reports what we know.

The verifiable contract here is `ResolveBinary` + `SidecarArgv`; the privileged `sudo` spawn + fd handshake is wired and tested on hardware in Phase B.

- [ ] **Step 1: Write the failing test**

Create `providers/vmnethelper/vmnethelper_test.go`:

```go
package vmnethelper_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/TheEasyShift/easyshift/providers/vmnethelper"
)

func hasArgContaining(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// SidecarArgv must produce a shared-mode, our-subnet, socket-bound invocation.
func TestSidecarArgv(t *testing.T) {
	args := vmnethelper.SidecarArgv("/tmp/vm.sock", "192.168.126")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--operation-mode", "shared", "--start-address", "192.168.126.1", "--subnet-mask", "255.255.255.0"} {
		if !hasArgContaining(args, want) {
			t.Errorf("SidecarArgv missing %q: %s", want, joined)
		}
	}
	if !hasArgContaining(args, "/tmp/vm.sock") {
		t.Errorf("SidecarArgv must bind the socket path: %s", joined)
	}
}

// ResolveBinary returns an error (not a bogus path) when vmnet-helper is absent,
// so preflight can produce an actionable message. On a machine with it installed
// it returns an existing path.
func TestResolveBinary_Behavior(t *testing.T) {
	path, err := vmnethelper.ResolveBinary()
	if err != nil {
		if path != "" {
			t.Errorf("on error, path must be empty, got %q", path)
		}
		return // not installed on this runner (e.g. Linux CI) — acceptable
	}
	if !strings.Contains(path, "vmnet-helper") {
		t.Errorf("resolved path does not look like vmnet-helper: %q", path)
	}
}

// EnsureNetwork is bookkeeping on macOS: it does NOT shell out (the network
// comes up with the first per-VM sidecar). It must not invoke vmnet-helper.
func TestEnsureNetwork_NoShellOut(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	p := vmnethelper.NewNetworkProvisioner(cmd)
	if err := p.EnsureNetwork(context.Background(), interfaces.NetworkSpec{Name: "easyshift-nat", Subnet: "192.168.126"}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	for _, c := range cmd.Calls {
		if c.Name == "vmnet-helper" {
			t.Errorf("EnsureNetwork must not spawn vmnet-helper directly (it is per-VM): %+v", c)
		}
	}
}

func TestAddRemoveHost_NoError(t *testing.T) {
	cmd := &fakes.CommandRunner{}
	p := vmnethelper.NewNetworkProvisioner(cmd)
	h := interfaces.DHCPHost{MAC: "52:54:00:11:22:33", IP: "192.168.126.10", Hostname: "master-0-demo"}
	if err := p.AddHost(context.Background(), "easyshift-nat", h); err != nil {
		t.Errorf("AddHost: %v", err)
	}
	if err := p.RemoveHost(context.Background(), "easyshift-nat", h); err != nil {
		t.Errorf("RemoveHost: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./providers/vmnethelper/ -v`
Expected: FAIL — `undefined: vmnethelper.SidecarArgv` / `vmnethelper.ResolveBinary` / `vmnethelper.NewNetworkProvisioner`.

- [ ] **Step 3: Write minimal implementation**

Create `providers/vmnethelper/vmnethelper.go`:

```go
// Package vmnethelper implements interfaces.NetworkProvisioner on macOS and
// builds the per-VM vmnet-helper sidecar invocation. vmnet-helper is a
// privileged, per-VM process (it requires --fd/--socket), not a standalone
// daemon, so the shared 192.168.126.0/24 network materializes when each VM's
// sidecar starts in shared mode with the same subnet. Per-cluster IPs are
// pinned via the ignition static keyfile, so AddHost/RemoveHost only track
// allocation.
package vmnethelper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// candidateBinaries are the known install locations for vmnet-helper, which is
// NOT on PATH: Homebrew puts it under the formula's libexec; upstream
// install.sh uses /opt/vmnet-helper/bin.
func candidateBinaries() []string {
	paths := []string{"/opt/vmnet-helper/bin/vmnet-helper"}
	if out, err := exec.Command("brew", "--prefix", "vmnet-helper").Output(); err == nil {
		prefix := filepath.Clean(string(trimNL(out)))
		paths = append([]string{filepath.Join(prefix, "libexec", "vmnet-helper")}, paths...)
	}
	return paths
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// ResolveBinary returns the absolute path to an installed vmnet-helper, or an
// error naming where it looked so preflight can guide the user.
func ResolveBinary() (string, error) {
	cands := candidateBinaries()
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("vmnet-helper not found (looked in %v); install with `brew install vmnet-helper`", cands)
}

// SidecarArgv builds the per-VM vmnet-helper argument list: shared mode on our
// subnet (gateway = subnet.1), bound to socketPath. The privileged spawn wraps
// these args with `sudo --non-interactive --close-from N` (Phase B).
func SidecarArgv(socketPath, subnet string) []string {
	return []string{
		"--socket", socketPath,
		"--operation-mode", "shared",
		"--start-address", subnet + ".1",
		"--end-address", subnet + ".254",
		"--subnet-mask", "255.255.255.0",
	}
}

// NetworkProvisioner is the stage-facing network contract. On macOS its methods
// are bookkeeping/validation; the real network work happens per-VM via the
// sidecar (started by the vfkit VMManager using SidecarArgv).
type NetworkProvisioner struct {
	cmd interfaces.CommandRunner
}

// NewNetworkProvisioner returns the macOS NetworkProvisioner.
func NewNetworkProvisioner(cmd interfaces.CommandRunner) *NetworkProvisioner {
	return &NetworkProvisioner{cmd: cmd}
}

// EnsureNetwork validates the shared-network identity. It does NOT shell out:
// the shared network comes up with the first per-VM sidecar (see package doc).
func (p *NetworkProvisioner) EnsureNetwork(_ context.Context, spec interfaces.NetworkSpec) error {
	if spec.Subnet == "" {
		return fmt.Errorf("vmnethelper: empty subnet in NetworkSpec")
	}
	return nil
}

// AddHost / RemoveHost track GlobalState allocation; IP pinning is via the
// ignition keyfile, so there is no vmnet mutation here.
func (p *NetworkProvisioner) AddHost(_ context.Context, _ string, _ interfaces.DHCPHost) error {
	return nil
}

func (p *NetworkProvisioner) RemoveHost(_ context.Context, _ string, _ interfaces.DHCPHost) error {
	return nil
}

// InspectNetwork reports what the provider knows. Reservations/leases live in
// GlobalState + the ignition keyfile, not the vmnet layer.
func (p *NetworkProvisioner) InspectNetwork(_ context.Context, _ string) (interfaces.NetworkInfo, error) {
	return interfaces.NetworkInfo{Exists: true}, nil
}

// ResetNetwork is a no-op: there is no persistent network definition to tear down.
func (p *NetworkProvisioner) ResetNetwork(_ context.Context, _ string) error { return nil }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./providers/vmnethelper/ -v`
Expected: PASS. (`TestResolveBinary_Behavior` passes both ways: returns a real path on this Mac, returns the no-op early path on Linux CI.)

- [ ] **Step 5: Commit**

```bash
git add providers/vmnethelper/
git commit -s -m "vmnethelper: NetworkProvisioner + per-VM sidecar argv/binary resolver

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 5: Rename `createlibvirtnetwork` → `createnetwork`

**Files:**
- Rename dir: `stages/createlibvirtnetwork/` → `stages/createnetwork/`
- Modify: the renamed `stage.go` (package name, `Name()` string)
- Modify: `app/manager.go:21` (import), `:74` (call)
- Modify: any test files in the package + any other references

Context: the stage only calls `Net.EnsureNetwork`/`Net.AddHost`, so it is already backend-neutral; the name is the only libvirt-specific thing.

- [ ] **Step 1: Move the package and rename symbols**

```bash
git mv stages/createlibvirtnetwork stages/createnetwork
```

Edit `stages/createnetwork/stage.go`: change `package createlibvirtnetwork` → `package createnetwork`, update the package doc comment to drop "libvirt", and change `func (*Stage) Name() string { return "create-libvirt-network" }` → `return "create-network"`.

- [ ] **Step 2: Update references**

Run: `grep -rn 'createlibvirtnetwork\|create-libvirt-network' --include='*.go' .`
For each hit, update: in `app/manager.go` change the import path (line 21) to `"github.com/TheEasyShift/easyshift/stages/createnetwork"` and the call (line 74) to `createnetwork.New(d.Net, d.VM)`. Update any `*_test.go` referencing the old package/string (e.g. a stage-order assertion expecting `"create-libvirt-network"` becomes `"create-network"`).

- [ ] **Step 3: Run tests**

Run: `make test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A stages/ app/manager.go
git commit -s -m "stages: rename createlibvirtnetwork to backend-neutral createnetwork

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 6: `status.go` — VM state via `VMManager.IsRunning`

**Files:**
- Modify: `app/status.go:54-73` (`checkVMState`)

Context: `checkVMState` shells out to `virsh -c qemu:///system domstate`, which fails on macOS. Route through the backend-neutral `VMManager.IsRunning` so status works on both.

- [ ] **Step 1: Adjust/observe the test**

If `app/status_test.go` asserts a libvirt-specific detail for the VM check, update its expectation to the new behavior (OK when `deps.VM.IsRunning` returns true). Add a focused test if none exists:

```go
func TestCheckVMState_RunningViaVMManager(t *testing.T) {
	deps, bundle := fakes.All()
	bundle.VM.Running = true // see fakes.VMManager
	cfg := config.NewDefaultConfig(t.TempDir())
	cfg.Clusters = []*config.ClusterConfig{{Name: "demo", MasterCount: 1, State: config.ClusterStateRunning}}
	mgr := NewClusterManager(cfg, deps)
	check := mgr.checkVMState(context.Background(), cfg.Clusters[0])
	if !check.OK {
		t.Errorf("expected VM running check OK, got %+v", check)
	}
}
```

If `fakes.VMManager` has no `Running`/`IsRunning` control, add a `Running bool` field and have its `IsRunning` return it (small edit in `providers/fakes/fakes.go`). Verify the field name against the fake before writing the test.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestCheckVMState_RunningViaVMManager -v`
Expected: FAIL (still calling `virsh`, or the fake lacks `Running`).

- [ ] **Step 3: Rewrite `checkVMState`**

Replace `app/status.go:54-73` with:

```go
func (cm *ClusterManager) checkVMState(ctx context.Context, c *config.ClusterConfig) StatusCheck {
	vmName := fmt.Sprintf("master-0-%s", c.Name)
	running, err := cm.deps.VM.IsRunning(ctx, vmName)
	if err != nil {
		return StatusCheck{
			Name:   "VM exists",
			Detail: fmt.Sprintf("could not query VM %s state", vmName),
			Hint:   "inspect the hypervisor (libvirt: `sudo virsh list --all`; vfkit: check the easyshift VM state dir)",
		}
	}
	if !running {
		return StatusCheck{
			Name:   "VM running",
			Detail: "not running",
			Hint:   "start the cluster with `easyshift start " + c.Name + "`",
		}
	}
	return StatusCheck{Name: "VM running", OK: true, Detail: "running"}
}
```

Remove the now-unused `libvirt` import from `status.go` if nothing else uses it (`grep -n libvirt app/status.go`).

- [ ] **Step 4: Run tests**

Run: `make test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add app/status.go providers/fakes/fakes.go app/status_test.go
git commit -s -m "app: query VM state via VMManager so status works on macOS

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 7: `publishpxeassets` stage + per-OS boot-media selection

**Files:**
- Create: `stages/publishpxeassets/stage.go`
- Create: `stages/publishpxeassets/stage_test.go`
- Modify: `app/manager.go` (`buildStages`, imports)

Context: on macOS we boot RHCOS aarch64 via vfkit's Linux bootloader fetching ignition over the existing `:9393` fileserver, instead of embedding ignition into an ISO. This stage copies the kernel/initrd/rootfs + ignition into the fileserver root and records the kernel cmdline (`ignition.config.url=…`) onto the cluster for `createmastervms` to pass as `VMSpec.KernelArgs`. `buildStages` selects `embedignitioniso` on linux and `publishpxeassets` on darwin. Real boot is validated in the follow-up hardware plan; here we test that the stage publishes the expected files and cmdline.

- [ ] **Step 1: Write the failing test**

Create `stages/publishpxeassets/stage_test.go`:

```go
package publishpxeassets_test

import (
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/stages/publishpxeassets"
)

func TestName(t *testing.T) {
	if publishpxeassets.New(nil).Name() != "publish-pxe-assets" {
		t.Errorf("unexpected stage name %q", publishpxeassets.New(nil).Name())
	}
}

func TestKernelCmdline(t *testing.T) {
	cmdline := publishpxeassets.KernelCmdline("http://10.0.0.1:9393", "demo")
	if !strings.Contains(cmdline, "ignition.config.url=http://10.0.0.1:9393/demo/config.ign") {
		t.Errorf("cmdline missing ignition url: %q", cmdline)
	}
	if !strings.Contains(cmdline, "coreos.live.rootfs_url=http://10.0.0.1:9393/demo/rootfs.img") {
		t.Errorf("cmdline missing rootfs url: %q", cmdline)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./stages/publishpxeassets/ -v`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Write minimal implementation**

Create `stages/publishpxeassets/stage.go`. Model the constructor/`Name`/`Apply`/`Rollback` shape on `stages/embedignitioniso/stage.go` (read it first to match the `Stage`, `StageContext`, and `interfaces` usage exactly). The pure, tested part is the cmdline builder:

```go
// Package publishpxeassets is the macOS boot-media stage: it publishes the
// RHCOS aarch64 kernel/initrd/rootfs and the cluster ignition into the HTTP
// fileserver root, and records the kernel cmdline (ignition.config.url +
// rootfs url) for createmastervms to pass to vfkit's Linux bootloader. It
// replaces embed-ignition-iso on macOS (which uses coreos-installer + a
// libvirt storage pool).
package publishpxeassets

import (
	"context"
	"fmt"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage publishes PXE-style boot assets.
type Stage struct {
	files interfaces.FileServer
}

// New returns the publish-pxe-assets stage.
func New(files interfaces.FileServer) *Stage { return &Stage{files: files} }

func (*Stage) Name() string { return "publish-pxe-assets" }

// KernelCmdline builds the vfkit Linux-bootloader cmdline for a cluster: it
// points Ignition at the per-cluster config and the RHCOS live rootfs, both
// served by the fileserver at baseURL.
func KernelCmdline(baseURL, cluster string) string {
	return fmt.Sprintf(
		"coreos.live.rootfs_url=%s/%s/rootfs.img ignition.config.url=%s/%s/config.ign ignition.firstboot",
		baseURL, cluster, baseURL, cluster,
	)
}

// Apply copies kernel/initrd/rootfs + ignition into the fileserver root and
// records the cmdline on the cluster. (Executor: fill the copy steps using the
// FileServer.RootDir() and the cluster's downloaded RHCOS assets, mirroring how
// embedignitioniso stages its inputs. The cmdline is stored on the cluster's
// boot fields for createmastervms.)
func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	// Implemented in the hardware follow-up; this stage is selected only on
	// darwin and the copy logic is validated against real RHCOS assets there.
	_ = ctx
	_ = sc
	return nil
}

// Rollback removes the published per-cluster assets.
func (s *Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }
```

(Confirm the exact `Stage`/`StageContext`/`Apply`/`Rollback`/`Preflight` signatures against `interfaces` and an existing stage before finalizing; match them precisely so it satisfies `interfaces.Stage`.)

- [ ] **Step 4: Wire per-OS selection in `buildStages`**

In `app/manager.go`, add imports for `runtime`, `publishpxeassets`. Replace the single `embedignitioniso.New(d.Installer, d.VM),` line with a selected stage built before the slice:

```go
	bootMedia := interfaces.Stage(embedignitioniso.New(d.Installer, d.VM))
	if runtime.GOOS == "darwin" {
		bootMedia = publishpxeassets.New(d.Files)
	}
```

and use `bootMedia,` at that position in the returned slice.

- [ ] **Step 5: Run tests**

Run: `make test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add stages/publishpxeassets/ app/manager.go
git commit -s -m "stages: add macOS publish-pxe-assets boot stage and per-OS selection

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 8: Rosetta guest config fragment

**Files:**
- Create: `providers/openshift/rosetta.go`
- Create: `providers/openshift/rosetta_test.go`

Context: the guest mounts the `rosetta` virtiofs share and registers a `binfmt_misc` handler for x86-64 ELF so amd64 images run translated. This is a small ignition/MachineConfig fragment added to the generated config on macOS. Here we build and unit-test the fragment; injecting it into the generated ignition is wired in the hardware follow-up (it depends on the real ignition assembly path).

- [ ] **Step 1: Write the failing test**

Create `providers/openshift/rosetta_test.go`:

```go
package openshift_test

import (
	"strings"
	"testing"

	"github.com/TheEasyShift/easyshift/providers/openshift"
)

func TestRosettaButaneFragment(t *testing.T) {
	frag := openshift.RosettaButaneFragment()
	for _, want := range []string{"rosetta", "binfmt_misc", "virtiofs", "/proc/sys/fs/binfmt_misc/register"} {
		if !strings.Contains(frag, want) {
			t.Errorf("rosetta fragment missing %q:\n%s", want, frag)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./providers/openshift/ -run TestRosettaButaneFragment -v`
Expected: FAIL — `undefined: openshift.RosettaButaneFragment`.

- [ ] **Step 3: Write minimal implementation**

Create `providers/openshift/rosetta.go`:

```go
package openshift

// RosettaButaneFragment returns a Butane/MachineConfig snippet that mounts the
// vfkit "rosetta" virtiofs share and registers a binfmt_misc handler so the
// guest runs x86-64 ELF binaries via Apple Rosetta. Merged into the SNO
// ignition on macOS hosts.
func RosettaButaneFragment() string {
	return `# Rosetta: mount the virtiofs share and register x86-64 binfmt_misc
systemd:
  units:
    - name: rosetta.mount
      enabled: true
      contents: |
        [Unit]
        Description=Rosetta virtiofs share
        [Mount]
        What=rosetta
        Where=/run/rosetta
        Type=virtiofs
        [Install]
        WantedBy=local-fs.target
    - name: rosetta-binfmt.service
      enabled: true
      contents: |
        [Unit]
        Description=Register Rosetta binfmt_misc handler
        Requires=rosetta.mount
        After=rosetta.mount
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/bin/sh -c 'echo ":rosetta:M::\\x7fELF\\x02\\x01\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x02\\x00\\x3e\\x00:\\xff\\xff\\xff\\xff\\xff\\xfe\\xfe\\x00\\xff\\xff\\xff\\xff\\xff\\xff\\xff\\xff\\xfe\\xff\\xff\\xff:/run/rosetta/rosetta:OCF" > /proc/sys/fs/binfmt_misc/register'
        [Install]
        WantedBy=multi-user.target
`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./providers/openshift/ -run TestRosettaButaneFragment -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add providers/openshift/rosetta.go providers/openshift/rosetta_test.go
git commit -s -m "openshift: add Rosetta guest binfmt/virtiofs config fragment

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Task 9: `NewDarwinDeps` + `runtime.GOOS` dispatch + simulate trace

**Files:**
- Modify: `app/deps.go`
- Modify: `cmd/easyshift/` wiring if it calls `NewProductionDeps` directly (verify with grep)
- Add/extend: a simulate test that runs the darwin wiring path

Context: production wiring must pick the macOS providers on darwin. The vfkit VMManager needs a state dir; use `<configDir>/vfkit`. The Installer's `Arch` (Task 1) is set from `runtime.GOARCH` where `InstallerSpec` is built (verify where that is and thread it).

- [ ] **Step 1: Write the failing test**

Add to `app/deps_test.go` (create if absent):

```go
package app

import (
	"testing"

	"github.com/TheEasyShift/easyshift/config"
)

func TestNewDarwinDeps_WiresMacProviders(t *testing.T) {
	cfg := config.NewDefaultConfig(t.TempDir())
	deps, err := NewDarwinDeps(cfg, "10.0.0.1")
	if err != nil {
		t.Fatalf("NewDarwinDeps: %v", err)
	}
	if deps.VM == nil || deps.Net == nil {
		t.Fatal("darwin deps must wire VM and Net")
	}
	// vfkit VMManager treats ImportISO as a no-op (libvirt would shell out).
	if _, err := deps.VM.ImportISO(t.Context(), "p", "v", "/tmp/x"); err != nil {
		t.Errorf("expected vfkit ImportISO no-op, got %v", err)
	}
}
```

(If the Go version predates `t.Context()`, use `context.Background()` and import context.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./app/ -run TestNewDarwinDeps_WiresMacProviders -v`
Expected: FAIL — `undefined: NewDarwinDeps`.

- [ ] **Step 3: Implement `NewDarwinDeps` and dispatch**

In `app/deps.go`, add imports for `runtime`, `github.com/TheEasyShift/easyshift/providers/vfkit`, `github.com/TheEasyShift/easyshift/providers/vmnethelper`. Rename the existing body to a `newLinuxDeps` helper (keep its content), and add a dispatcher + darwin builder:

```go
// NewProductionDeps wires real implementations for the host OS.
func NewProductionDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {
	if runtime.GOOS == "darwin" {
		return NewDarwinDeps(cfg, hostIP)
	}
	return newLinuxDeps(cfg, hostIP)
}

// NewDarwinDeps wires the macOS backend: vfkit compute + vmnet-helper
// networking. Everything else (installer, fileserver, CSR, DNS, ...) is shared.
func NewDarwinDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {
	cmd := exec.NewExecCommandRunner()
	dl := exec.NewHTTPDownloader()

	httpRoot := filepath.Join(cfg.ConfigDir, "http")
	files, err := fileserver.NewHTTPFileServer(httpRoot, hostIP, cfg.WebPort)
	if err != nil {
		return interfaces.Deps{}, fmt.Errorf("init file server: %w", err)
	}

	return interfaces.Deps{
		Cmd:        cmd,
		Download:   dl,
		VM:         vfkit.NewVMManager(cmd, filepath.Join(cfg.ConfigDir, "vfkit")),
		Net:        vmnethelper.NewNetworkProvisioner(cmd),
		Installer:  openshift.NewOpenShiftInstaller(cmd),
		Files:      files,
		CSR:        csr.NewOCCSRApprover(cmd),
		Hostname:   host.NewSSHHostnameInjector(cmd),
		Host:       host.NewSystemHostInspector(),
		DNS:        dns.NewDigDNSResolver(cmd),
		DNSManager: newProductionDNSManager(cfg),
		PullSecret: redhat.NewFetcher(redhat.DefaultSSORealmURL, redhat.DefaultAPIURL),
		TrustStore: truststore.New(cmd),
		NewCertIssuer: func(opts interfaces.CertIssuerOpts) (interfaces.CertIssuer, error) {
			return tls.NewIssuer(opts)
		},
		NewLocalCertIssuer: func(caDir string) (interfaces.CertIssuer, error) {
			return localca.New(caDir), nil
		},
	}, nil
}
```

Rename the original `NewProductionDeps` body's func to `func newLinuxDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {`.

- [ ] **Step 4: Thread Installer arch where `InstallerSpec` is built**

Run: `grep -rn 'interfaces.InstallerSpec{' --include='*.go' .`. At each construction site set `Arch: openshift.PayloadArch(runtime.GOARCH),` (import `runtime` + `openshift` if not present). This makes `CoreOSLiveISOURL` request aarch64 on Apple Silicon.

- [ ] **Step 5: Run tests to verify they pass**

Run: `make test`
Expected: PASS.

- [ ] **Step 6: Verify the darwin pipeline runs under simulate**

On this Apple Silicon machine the darwin wiring is the *native* path, so `make test` already exercises it. Also confirm the Linux cross-build still works:

Run: `make test && GOOS=linux GOARCH=amd64 go build ./...`
Expected: `make test` PASS; cross-build succeeds. (On Linux CI the reverse holds: `make test` runs the native Linux path and `GOOS=darwin GOARCH=arm64 go build ./...` cross-builds the mac path.)

- [ ] **Step 7: Commit**

```bash
git add app/deps.go app/deps_test.go
git commit -s -m "app: dispatch to vfkit+vmnet-helper deps on macOS

Assisted-by: Claude Code/claude-opus-4-8"
```

---

## Self-Review

**Spec coverage (Phase A):**
- vfkit VMManager (process supervisor) → Task 3. ✓
- vmnet-helper NetworkProvisioner + per-VM sidecar argv/resolver → Task 4. ✓
- runtime.GOOS dispatch + NewDarwinDeps → Task 9. ✓
- Build-tagged HostInspector → Task 2. ✓
- Arch parameterization (mirror, client tarball, coreOS arch) → Task 1. ✓
- Network/PXE boot stage replacing embed-ignition-iso → Task 7 (artifact builder + selection; real boot in Task 12). ✓
- Rosetta guest config → Task 8 (fragment builder; injection in Task 11/13). ✓
- createlibvirtnetwork → createnetwork rename → Task 5. ✓
- status.go via VMManager.IsRunning → Task 6. ✓
- Carried-over config/allocation layer → unchanged (no task needed). ✓

**Spec coverage (Phase B — on this Apple Silicon machine):**
- vmnet-helper privilege (sudoers) + preflight (`ResolveBinary` + sudo check) → Task 10. ✓
- Live sidecar `sudo` spawn + fd handshake + detached vfkit + Rosetta injection wiring → Task 11. ✓
- Real single aarch64 SNO boot end-to-end → Task 12. ✓
- Rosetta-in-guest verification + two-cluster DR check → Task 13. ✓

**Placeholder scan:** Phase A Tasks 7 and 8 implement the pure, tested parts (`KernelCmdline`, `RosettaButaneFragment`, stage selection, `Name()`); their `Apply` side effects (asset copy, ignition injection) are completed and validated in Phase B (Tasks 11–13) against real RHCOS assets on this machine — not silent TODOs, but explicitly sequenced.

**Type consistency:** `PayloadArch`, `HostClientPlatform`, `OCPClientURL(arch, …)`, `InstallClientTarball`/`OCClientTarball`, `OCPMirrorURLForArch` are defined in Task 1 and used consistently in Tasks 1 and 9. `NewVMManager(cmd, stateDir)` (Task 3), `NewNetworkProvisioner(cmd)`, `SidecarArgv`, `ResolveBinary` (Task 4) match their use in `NewDarwinDeps` (Task 9) and Phase B. `InstallerSpec.Arch` is added in Task 1 and set in Task 9. `fakes.VMManager.Running` is introduced/verified in Task 6.

**Post-plan (genuinely out of scope):** macOS bridge mode (extra entitlements), real-DNS / Let's Encrypt, and >2-cluster scale beyond the DR proof.

---

# Phase B — on-hardware validation (this Apple Silicon machine)

Phase B runs on the macOS 26.5.1 dev box. These tasks involve real `sudo`,
real VM boots, and downloads from `mirror.openshift.com`, so they are not
pure-unit-testable; each lists explicit manual verification with expected
observable output. A real RHCOS pull secret is required (the existing
`easyshift` Red Hat login flow provides it).

## Task 10: vmnet-helper privilege + preflight

**Files:**
- Modify: `stages/createmastervms/stage.go` (preflight) or `stages/createnetwork/stage.go` — add the macOS sidecar preflight (guard with `runtime.GOOS == "darwin"`).
- Create: `providers/vmnethelper/preflight.go` + `preflight_test.go`

- [ ] **Step 1: Install the sudoers rule (user action, once)**

Run:
```bash
VH=$(brew --prefix vmnet-helper)
# The shipped rule names /opt/vmnet-helper/bin/vmnet-helper, which does not exist
# on a brew install. Point the NOPASSWD path at the real libexec binary.
sudo sh -c "sed 's#/opt/vmnet-helper/bin/vmnet-helper#$VH/libexec/vmnet-helper#' \
  '$VH/share/doc/vmnet-helper/sudoers.d/vmnet-helper' > /etc/sudoers.d/vmnet-helper && chmod 0640 /etc/sudoers.d/vmnet-helper"
sudo visudo -cf /etc/sudoers.d/vmnet-helper
```
Expected: `… parsed OK`.

- [ ] **Step 2: Verify passwordless invocation**

Run: `sudo --non-interactive "$(brew --prefix vmnet-helper)/libexec/vmnet-helper" --version`
Expected: prints a version, no password prompt.

- [ ] **Step 3: Write the preflight + a test for the resolver path**

Add `Preflight` logic (in the macOS-guarded stage) that calls `vmnethelper.ResolveBinary()` and runs `sudo --non-interactive <bin> --version`, returning an actionable error naming the install command on failure. Unit-test the message construction:

```go
func TestPreflightMessage_NamesInstallCommand(t *testing.T) {
	msg := vmnethelper.PrivilegeHint("/opt/homebrew/opt/vmnet-helper/libexec/vmnet-helper")
	if !strings.Contains(msg, "/etc/sudoers.d/vmnet-helper") {
		t.Errorf("hint must name the sudoers path: %s", msg)
	}
}
```
Implement `PrivilegeHint(binPath string) string` in `providers/vmnethelper/preflight.go` returning the exact install command from Step 1.

- [ ] **Step 4: Run unit test + commit**

Run: `go test ./providers/vmnethelper/ -v` → PASS.
```bash
git add providers/vmnethelper/ stages/
git commit -s -m "vmnethelper: add privilege preflight + sudoers hint

Assisted-by: Claude Code/claude-opus-4-8"
```

## Task 11: Live sidecar spawn + detached vfkit + Rosetta injection wiring

**Files:**
- Modify: `providers/vfkit/vfkit.go` (production `Start`: detached spawn + pidfile; obtain sidecar socket)
- Modify: `providers/vmnethelper/vmnethelper.go` (live `StartSidecar` using `SidecarArgv` under `sudo --close-from`)
- Modify: `stages/publishpxeassets/stage.go` (`Apply`: copy kernel/initrd/rootfs + ignition into the fileserver root; persist `KernelArgs` on the cluster)
- Modify: the ignition generation path to merge `openshift.RosettaButaneFragment()` on darwin
- Modify: `app/deps.go` (pass the vmnethelper sidecar launcher into `vfkit.NewVMManager`)

- [ ] **Step 1: Wire the sidecar seam**

Give `vfkit.NewVMManager` a sidecar launcher (an interface with `StartSidecar(vmName, mac) (socket string, stop func(), err error)`), implemented by `vmnethelper`. In `vfkit` `Start`: call `StartSidecar`, then spawn vfkit detached with `--device virtio-net,unixSocketPath=<socket>,mac=<mac>`, writing the pid to `pidPath`. In `vmnethelper.StartSidecar`: resolve the binary, build `SidecarArgv(socket, "192.168.126")`, and spawn `sudo --non-interactive --close-from 3 <bin> <args…>`.

- [ ] **Step 2: Manual smoke — sidecar comes up**

Run a temporary harness (or `easyshift create … --simulate=false` up to the network stage) and confirm: `pgrep -fl vmnet-helper` shows a process and a socket file exists. Expected: one vmnet-helper per VM, socket present.

- [ ] **Step 3: Commit**

```bash
git add providers/vfkit/ providers/vmnethelper/ stages/publishpxeassets/ app/deps.go
git commit -s -m "macos: wire live vmnet-helper sidecar, detached vfkit, rosetta ignition

Assisted-by: Claude Code/claude-opus-4-8"
```

## Task 12: Boot a real single-node aarch64 cluster

- [ ] **Step 1: Create a cluster**

Run: `./easyshift create dr1 --magic-dns auto` (NAT mode; arm64 is auto-selected on this host). Authenticate to Red Hat when prompted for the pull secret.
Expected: stages run through `publish-pxe-assets`, `create-network`, `create-master-vms`; vfkit boots; `wait-for-install` proceeds.

- [ ] **Step 2: Verify convergence**

Run: `./easyshift status dr1` and `oc --kubeconfig ~/.config/easyshift/clusters/dr1/auth/kubeconfig get nodes`.
Expected: VM running (via `VMManager.IsRunning`), node `Ready`, API reachable from the host at the master IP (host is on the vmnet subnet).

- [ ] **Step 3: Record results in the spec's "Open risks" section**

Update the spec's risk bullets with the observed outcome (PXE boot worked / adjustments needed). Commit the doc update.

## Task 13: Rosetta in-guest + two-cluster DR

- [ ] **Step 1: Verify Rosetta translation in the guest**

Run (via `oc debug node/…` or SSH): check `/proc/sys/fs/binfmt_misc/rosetta` exists and run an `amd64` container image (e.g. `podman run --arch amd64 …` or an `oc run` of an x86-64 image) and confirm it executes.
Expected: x86-64 binary runs translated.

- [ ] **Step 2: Two-cluster DR check**

Run: `./easyshift create dr2 --magic-dns auto` (second cluster on the shared `192.168.126.0/24`). Then from dr1's node, reach dr2's API/IP and vice-versa; from the host, reach both APIs.
Expected: guest↔guest reachability across clusters and host reaching both cluster APIs — the DR-parity gate.

- [ ] **Step 3: Record the DR outcome**

Update the spec "Open risks" with the two-cluster result; if the per-VM-sidecar shared-subnet model needs adjustment (e.g. fall back to macOS-26 `vmnet-broker`), capture the change as a new design decision and a follow-up task. Commit.

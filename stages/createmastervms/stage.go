// Package createmastervms provisions the master VM(s) that boot from the
// embedded SNO ISO (bootstrap-in-place).
package createmastervms

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage creates the cluster's master VMs.
type Stage struct {
	vm   interfaces.VMManager
	host interfaces.HostInspector
}

// New returns the create-master-vms stage.
func New(vm interfaces.VMManager, host interfaces.HostInspector) *Stage {
	return &Stage{vm: vm, host: host}
}

func (*Stage) Name() string { return "create-master-vms" }

// Preflight runs every host-environment check needed before VM creation:
// libvirt reachable, storage pool active, virt-install on PATH, CPU
// virtualization, enough disk, and (bridge mode) a usable host bridge.
func (s *Stage) Preflight(ctx context.Context, sc *interfaces.StageContext) error {
	if err := s.vm.CheckAccess(ctx); err != nil {
		return err
	}
	if err := s.vm.StoragePoolActive(ctx, sc.Cluster.StoragePool); err != nil {
		return err
	}
	// macOS boots via vfkit (no virt-install / libvirt storage pool); Linux
	// needs virt-install on PATH.
	bootBinary := "virt-install"
	if runtime.GOOS == "darwin" {
		bootBinary = "vfkit"
	}
	if err := s.host.LookPath(bootBinary); err != nil {
		return err
	}
	hasVT, err := s.host.HasCPUVirtualization()
	if err != nil {
		return fmt.Errorf("detect cpu virtualization: %w", err)
	}
	if !hasVT {
		return fmt.Errorf("host CPU does not advertise vmx/svm — virtualization extensions are required")
	}
	if err := s.checkHostMemory(ctx, sc); err != nil {
		return err
	}
	avail, err := s.host.AvailableDiskBytes(sc.Config.ConfigDir)
	if err != nil {
		return fmt.Errorf("query disk space at %s: %w", sc.Config.ConfigDir, err)
	}
	need := uint64(sc.Cluster.MasterDiskGB) * 1024 * 1024 * 1024
	// Baking attaches a per-cluster copy of the store disk; count it — except
	// on macOS, where ImportDisk APFS-clones the cached image and the copy
	// costs no space until modified (it never is: the guest mounts it ro).
	// The ODF data disk is sparse on both backends (a truncate-sparse raw file
	// on macOS, qcow2 on Linux) so it costs no upfront space.
	if sc.Cluster.BakeImages && runtime.GOOS != "darwin" {
		if fi, err := os.Stat(config.ImageStoreDiskPath(sc.Config.ConfigDir, sc.Cluster.OCPVersion)); err == nil {
			need += uint64(fi.Size())
		}
	}
	if avail < need {
		return fmt.Errorf("insufficient disk under %s: have %d GiB, need %d GiB for master disk%s",
			sc.Config.ConfigDir, avail>>30, need>>30, bakeNote(sc.Cluster.BakeImages))
	}
	if sc.Cluster.NetworkMode == config.NetworkModeBridge {
		br, err := s.host.InspectBridge(sc.Cluster.Bridge)
		if err != nil {
			return fmt.Errorf("inspect bridge %s: %w", sc.Cluster.Bridge, err)
		}
		if !br.Exists {
			return fmt.Errorf("bridge %q does not exist (or is not a Linux bridge) on this host; create it and enslave your LAN interface before running easyshift", sc.Cluster.Bridge)
		}
		if len(br.Slaves) == 0 {
			return fmt.Errorf("bridge %q exists but has no slave interfaces — VMs attached to it have no path to the LAN; enslave your LAN NIC (e.g. `sudo nmcli con add type bridge-slave ifname <NIC> master %s`)", sc.Cluster.Bridge, sc.Cluster.Bridge)
		}
		if !br.Up {
			return fmt.Errorf("bridge %q is not up (operstate != \"up\") with slaves %v; bring it up (e.g. `sudo ip link set %s up`)", sc.Cluster.Bridge, br.Slaves, sc.Cluster.Bridge)
		}
	}
	return nil
}

// hostMemoryReserveMiB is what the host OS keeps for itself when sizing VMs.
// Calibrated on hardware (24 GiB Mac mini): a 19456 MiB VM ran fine, a
// 20480 MiB VM thrashed the host into a load-300 swap spiral — and
// 24576 - 5120 = 19456. This same check also catches requests beyond
// Virtualization.framework's per-VM cap (e.g. the 32768 default on a 24 GiB
// host), which would otherwise crash-loop vfkit at launch.
const hostMemoryReserveMiB = 5120

// checkHostMemory refuses to create a master whose RAM, plus the RAM of
// every other easyshift cluster whose master VM is currently running, would
// exceed physical memory minus the host reserve. Oversubscribing does not
// fail cleanly — it thrashes the whole host — so this blocks with the fix in
// the message instead of warning. Liveness is probed per VM (the persisted
// cluster state can be stale). A failed physical-memory probe skips the
// check: it is a guard, not a gate on exotic hosts.
func (s *Stage) checkHostMemory(ctx context.Context, sc *interfaces.StageContext) error {
	physBytes, err := s.host.PhysicalMemoryBytes()
	if err != nil || physBytes == 0 {
		return nil
	}
	physMiB := physBytes >> 20
	totalMiB := uint64(sc.Cluster.MasterRAM)
	var running []string
	for _, other := range sc.Config.Clusters {
		if other.Name == sc.Cluster.Name {
			continue
		}
		up, err := s.vm.IsRunning(ctx, fmt.Sprintf("master-0-%s", other.Name))
		if err != nil || !up {
			continue
		}
		totalMiB += uint64(other.MasterRAM)
		running = append(running, other.Name)
	}
	if totalMiB+hostMemoryReserveMiB <= physMiB {
		return nil
	}
	if len(running) > 0 {
		return fmt.Errorf("not enough host memory: this master (%d MiB) plus running cluster(s) %v (%d MiB total) exceeds physical %d MiB minus the %d MiB host reserve; stop one first (e.g. `easyshift stop %s`) or lower --master-ram",
			sc.Cluster.MasterRAM, running, totalMiB, physMiB, hostMemoryReserveMiB, running[0])
	}
	return fmt.Errorf("not enough host memory: --master-ram %d MiB exceeds physical %d MiB minus the %d MiB host reserve (max usable: %d MiB); lower --master-ram",
		sc.Cluster.MasterRAM, physMiB, hostMemoryReserveMiB, physMiB-hostMemoryReserveMiB)
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	for i := 0; i < sc.Cluster.MasterCount; i++ {
		if err := s.createMasterVM(ctx, sc, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stage) Rollback(ctx context.Context, sc *interfaces.StageContext) error {
	for i := sc.Cluster.MasterCount - 1; i >= 0; i-- {
		name := fmt.Sprintf("master-%d-%s", i, sc.Cluster.Name)
		if err := s.vm.Delete(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stage) createMasterVM(ctx context.Context, sc *interfaces.StageContext, index int) error {
	c := sc.Cluster
	role := fmt.Sprintf("master-%d", index)
	vmName := fmt.Sprintf("%s-%s", role, c.Name)
	mac := macFor(c, role)
	var extraDisks []interfaces.ExtraDisk
	if c.BakeImages {
		disk, err := s.attachImageStore(ctx, sc, vmName)
		if err != nil {
			return err
		}
		extraDisks = append(extraDisks, disk)
	}
	if c.ODF {
		path, err := s.vm.CreateDataDisk(ctx, c.StoragePool, config.ODFVolName(vmName), c.ODFDiskGB)
		if err != nil {
			return fmt.Errorf("create odf data disk: %w", err)
		}
		// Writable and exclusive; attached after the bake store so the guest
		// device path is deterministic (/dev/vdb, or /dev/vdc with baking).
		extraDisks = append(extraDisks, interfaces.ExtraDisk{Path: path})
	}
	spec := interfaces.VMSpec{
		Name:        vmName,
		MemoryMiB:   c.MasterRAM,
		VCPUs:       c.MasterCPUs,
		DiskSizeGiB: c.MasterDiskGB,
		StoragePool: c.StoragePool,
		MAC:         mac,
		ExtraDisks:  extraDisks,
	}
	if runtime.GOOS == "darwin" {
		// vfkit install phase: direct-kernel boot of the live PXE assets with
		// the network-ignition cmdline published by publish-pxe-assets. The
		// network attaches via the vmnet-helper sidecar, not a --network arg.
		spec.KernelPath = sc.RHCOSKernelPath()
		spec.InitrdPath = sc.RHCOSInitramfsPath()
		spec.KernelArgs = c.InstallKernelCmdline
	} else {
		spec.NetworkArg = networkArgFor(c, mac)
		spec.BootISO = c.BootISOVolPath
	}
	return s.vm.Create(ctx, spec)
}

// attachImageStore uploads the cached, multi-arch baked store qcow2 into the
// pool as a per-cluster volume (so cluster delete, which removes all of a
// domain's storage, never strands another cluster) and returns it as a
// read-only, shareable extra disk. The node mounts it by label and points
// CRI-O's additionalimagestores at it.
func (s *Stage) attachImageStore(ctx context.Context, sc *interfaces.StageContext, vmName string) (interfaces.ExtraDisk, error) {
	// ImportDisk stats the source and returns a clear error if the bake-image-
	// store stage never produced it, so no extra guard is needed here (and the
	// raw stat would wrongly fail under --simulate, where no real file exists).
	cached := config.ImageStoreDiskPath(sc.Config.ConfigDir, sc.Cluster.OCPVersion)
	volPath, err := s.vm.ImportDisk(ctx, sc.Cluster.StoragePool, config.ImageStoreVolName(vmName), cached)
	if err != nil {
		return interfaces.ExtraDisk{}, fmt.Errorf("import baked image store into pool: %w", err)
	}
	return interfaces.ExtraDisk{Path: volPath, ReadOnly: true, Shareable: true}, nil
}

// bakeNote annotates the disk-space error when the baked image store inflates
// the requirement, so the number isn't surprising.
func bakeNote(baking bool) string {
	if baking {
		return " (incl. baked image store)"
	}
	return ""
}

func macFor(c *config.ClusterConfig, role string) string {
	for i, mac := range c.MACAddresses {
		if i < c.MasterCount && role == fmt.Sprintf("master-%d", i) {
			return mac
		}
		if i >= c.MasterCount && role == fmt.Sprintf("worker-%d", i-c.MasterCount) {
			return mac
		}
	}
	return ""
}

// networkArgFor builds the `virt-install --network` arg. model=virtio is
// forced because virt-install's default (e1000) hangs the Tx queue under
// load on modern kernels, stranding the VM mid-bootstrap. NAT-mode VMs all
// attach to the single shared NAT network so clusters can reach each other.
func networkArgFor(c *config.ClusterConfig, mac string) string {
	if c.NetworkMode == config.NetworkModeBridge {
		return fmt.Sprintf("bridge=%s,mac=%s,model=virtio", c.Bridge, mac)
	}
	return fmt.Sprintf("network=%s,mac=%s,model=virtio", config.SharedNATNetwork, mac)
}

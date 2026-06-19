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
	avail, err := s.host.AvailableDiskBytes(sc.Config.ConfigDir)
	if err != nil {
		return fmt.Errorf("query disk space at %s: %w", sc.Config.ConfigDir, err)
	}
	need := uint64(sc.Cluster.MasterDiskGB) * 1024 * 1024 * 1024
	// Baking attaches a per-cluster copy of the store qcow2; count it.
	if sc.Cluster.BakeImages {
		if fi, err := os.Stat(config.ImageStoreQcowPath(sc.Config.ConfigDir, sc.Cluster.OCPVersion)); err == nil {
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
	cached := config.ImageStoreQcowPath(sc.Config.ConfigDir, sc.Cluster.OCPVersion)
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

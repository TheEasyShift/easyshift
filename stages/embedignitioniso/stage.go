// Package embedignitioniso produces the per-cluster master ISO by embedding
// the SNO ignition into the cached live ISO, then uploads it to the libvirt
// storage pool so qemu can boot from it.
package embedignitioniso

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage builds and uploads the master boot ISO.
type Stage struct {
	installer interfaces.Installer
	vm        interfaces.VMManager
}

// New returns the embed-ignition-iso stage.
func New(installer interfaces.Installer, vm interfaces.VMManager) *Stage {
	return &Stage{installer: installer, vm: vm}
}

func (*Stage) Name() string { return "embed-ignition-iso" }

// Preflight verifies the storage pool is ready before uploading the ISO.
func (s *Stage) Preflight(ctx context.Context, sc *interfaces.StageContext) error {
	return s.vm.StoragePoolActive(ctx, sc.Cluster.StoragePool)
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	srcISO := sc.RHCOSLiveISOPath()
	ignition := filepath.Join(sc.ClusterDir(), "bootstrap-in-place-for-live-iso.ign")
	local := sc.MasterISOPath()
	// When baking, also wire the live-ISO (bootstrap) phase to the read-only
	// store so the temporary control plane serves images locally too. Done on
	// the generated ignition before it is embedded into the ISO.
	if sc.Cluster.BakeImages {
		if err := s.installer.MergeImageStoreIntoLiveISOIgnition(ctx, sc.InstallerSpec(), ignition); err != nil {
			return err
		}
	}
	if err := s.installer.EmbedIgnitionInISO(ctx, sc.InstallerSpec(), srcISO, ignition, local); err != nil {
		return err
	}
	// Pin the master's address statically from first boot in BOTH network
	// modes. The node's IP would otherwise come from DHCP and can race: grab a
	// pool address before the reservation takes effect, and etcd/kubelet bake
	// the wrong nodeIP in permanently. Bridge mode races the user's router;
	// NAT mode races libvirt's dnsmasq (a reservation not yet propagated when
	// the VM DISCOVERs leaves the master on a sticky dynamic lease). Embedding a
	// NetworkManager keyfile that pins the reserved IP removes the DHCP-timing
	// dependency entirely. The hostname is set separately by the SSH injector
	// (wait-for-install), since a static NIC never receives DHCP option 12.
	keyfile, err := sc.Cluster.StaticNetworkKeyfile()
	if err != nil {
		return fmt.Errorf("render static network keyfile: %w", err)
	}
	keyfilePath := filepath.Join(sc.ClusterDir(), "master.nmconnection")
	if err := os.WriteFile(keyfilePath, []byte(keyfile), 0o600); err != nil {
		return fmt.Errorf("write network keyfile: %w", err)
	}
	if err := s.installer.EmbedNetworkKeyfileInISO(ctx, sc.InstallerSpec(), keyfilePath, local); err != nil {
		return err
	}
	// Upload into the pool so qemu (not just the easyshift user) can read it.
	volPath, err := s.vm.ImportISO(ctx, sc.Cluster.StoragePool, sc.MasterISOVolName(), local)
	if err != nil {
		return err
	}
	sc.Cluster.BootISOVolPath = volPath
	return sc.Config.Save()
}

func (s *Stage) Rollback(ctx context.Context, sc *interfaces.StageContext) error {
	_ = s.vm.RemoveISO(ctx, sc.Cluster.StoragePool, sc.MasterISOVolName())
	_ = os.Remove(sc.MasterISOPath())
	sc.Cluster.BootISOVolPath = ""
	return sc.Config.Save()
}

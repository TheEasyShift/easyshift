// Package createnetwork attaches a NAT-mode cluster to the single shared NAT
// network: it ensures that network exists (a host-global resource) and adds
// this cluster's master DHCP reservation. In bridge mode every method is a
// no-op. The implementation is backend-neutral (it only uses the
// NetworkProvisioner interface), so it serves both libvirt and vmnet-helper.
package createnetwork

import (
	"context"
	"fmt"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage ensures the shared NAT network and the cluster's reservation.
type Stage struct {
	net interfaces.NetworkProvisioner
	vm  interfaces.VMManager
}

// New returns the create-network stage. vm is used only for the
// hypervisor-reachability preflight.
func New(net interfaces.NetworkProvisioner, vm interfaces.VMManager) *Stage {
	return &Stage{net: net, vm: vm}
}

func (*Stage) Name() string { return "create-network" }

// Preflight ensures qemu:///system is reachable before NAT-mode side effects.
func (s *Stage) Preflight(ctx context.Context, sc *interfaces.StageContext) error {
	if sc.Cluster.NetworkMode != config.NetworkModeNAT {
		return nil
	}
	return s.vm.CheckAccess(ctx)
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	if sc.Cluster.NetworkMode != config.NetworkModeNAT {
		return nil
	}
	c := sc.Cluster
	// Ensure the single shared NAT network exists. Idempotent — only the
	// first NAT cluster actually creates it; the rest just confirm it's up.
	// No <domain> (magic DNS forwards wildcard-service queries upstream).
	if err := s.net.EnsureNetwork(ctx, interfaces.NetworkSpec{
		Name:   config.SharedNATNetwork,
		Subnet: config.BaseNetworkRange,
	}); err != nil {
		return err
	}
	// Add THIS cluster's master reservation: pins the IP and hands the VM a
	// unique hostname via DHCP option 12 (so node-valid-hostname is satisfied
	// without an SSH injector). The hostname must be unique on the shared
	// network, so it carries the cluster name.
	return s.net.AddHost(ctx, config.SharedNATNetwork, masterReservation(c))
}

func (s *Stage) Rollback(ctx context.Context, sc *interfaces.StageContext) error {
	if sc.Cluster.NetworkMode != config.NetworkModeNAT {
		return nil
	}
	// Remove only this cluster's reservation — the shared network is global
	// and stays for the other clusters.
	return s.net.RemoveHost(ctx, config.SharedNATNetwork, masterReservation(sc.Cluster))
}

func masterReservation(c *config.ClusterConfig) interfaces.DHCPHost {
	return interfaces.DHCPHost{
		MAC:      firstOrEmpty(c.MACAddresses),
		IP:       c.PrimaryMasterIP(),
		Hostname: fmt.Sprintf("master-0-%s", c.Name),
	}
}

func firstOrEmpty(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

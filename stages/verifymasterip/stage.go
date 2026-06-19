// Package verifymasterip aborts a bridge-mode install early if the master VM
// did not come up on the IP reserved for it at the router.
//
// In bridge mode the node's address comes from the user's router DHCP, not
// from libvirt, so there is a race: if the node grabs a pool address before
// the MAC reservation takes effect, etcd bootstraps on that wrong IP and bakes
// it into the cluster permanently (the apiservers then crash-loop forever,
// unable to reach etcd at an address nothing answers on). That failure is only
// visible hours into the install. This stage runs the moment the VM has booted
// — before the bootstrap control plane brings up etcd — and fails fast so the
// user can fix the reservation and re-run, instead of losing hours. NAT mode
// uses libvirt's own deterministic reservation, so the check is skipped there.
package verifymasterip

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

const (
	// defaultTimeout bounds how long we wait for the reserved IP to come up.
	// Comfortably longer than RHCOS live-ISO DHCP (~1-3 min) yet well short of
	// when the bootstrap control plane starts etcd (~15-20 min in), so a wrong
	// IP is caught before it can be committed.
	defaultTimeout = 8 * time.Minute
	defaultPoll    = 10 * time.Second
	defaultDial    = 3 * time.Second
	sshPort        = "22" // RHCOS live ISO runs sshd; cheapest early liveness probe.
)

// Stage verifies the master VM acquired its reserved IP (bridge mode only).
type Stage struct {
	host interfaces.HostInspector
	// timeout/poll/dial are overridable in tests; New sets production defaults.
	timeout time.Duration
	poll    time.Duration
	dial    time.Duration
}

// New returns the verify-master-ip stage.
func New(host interfaces.HostInspector) *Stage {
	return &Stage{host: host, timeout: defaultTimeout, poll: defaultPoll, dial: defaultDial}
}

func (*Stage) Name() string { return "verify-master-ip" }

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	c := sc.Cluster
	if c.NetworkMode != config.NetworkModeBridge {
		// NAT mode: libvirt's DHCP reservation is deterministic, nothing to verify.
		return nil
	}
	return s.verify(ctx, c.MasterMAC, c.MasterIP)
}

func (*Stage) Rollback(_ context.Context, _ *interfaces.StageContext) error { return nil }

// verify polls until the reserved IP is positively confirmed live on the
// master's MAC, fails fast if the MAC is found live on a different IP, and
// otherwise gives up after the timeout. Every decision requires a successful
// TCP connect so a stale /proc/net/arp entry can't produce a false verdict in
// either direction.
func (s *Stage) verify(ctx context.Context, mac, wantIP string) error {
	logrus.Infof("verifying master VM came up on its reserved IP %s (MAC %s)", wantIP, mac)
	wantAddr := net.JoinHostPort(wantIP, sshPort)
	deadline := time.Now().Add(s.timeout)
	for {
		// Dialing wantIP also forces an ARP exchange, so a node actually at
		// wantIP gets recorded as wantIP<->mac for the lookup below.
		liveAtWant := s.host.DialTCP(wantAddr, s.dial) == nil
		gotIP, _ := s.host.ARPLookup(mac)

		switch {
		case liveAtWant && gotIP == wantIP:
			logrus.Infof("master VM confirmed at reserved IP %s", wantIP)
			return nil
		case gotIP != "" && gotIP != wantIP:
			// MAC seen at a different IP — confirm it's a live node (not a
			// stale ARP entry) before failing the install.
			if s.host.DialTCP(net.JoinHostPort(gotIP, sshPort), s.dial) == nil {
				return mismatchErr(mac, gotIP, wantIP)
			}
		}

		if !time.Now().Before(deadline) {
			if gotIP != "" && gotIP != wantIP {
				return mismatchErr(mac, gotIP, wantIP)
			}
			return fmt.Errorf("master VM (MAC %s) did not come up on its reserved IP %s within %s; "+
				"the node likely received a different DHCP address, and continuing would let etcd "+
				"bootstrap on it and pin the cluster to the wrong address permanently. Verify the router "+
				"DHCP reservation maps %s -> %s (and that no other host holds %s), then re-run "+
				"`easyshift create`", mac, wantIP, s.timeout, mac, wantIP, wantIP)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.poll):
		}
	}
}

func mismatchErr(mac, gotIP, wantIP string) error {
	return fmt.Errorf("master VM (MAC %s) came up on %s, but its reserved IP is %s; "+
		"continuing would let etcd bootstrap on %s and pin the cluster to the wrong address permanently "+
		"(the apiservers could never reach etcd). Fix the router DHCP reservation (%s -> %s) so the node "+
		"gets %s on first boot, then re-run `easyshift create`",
		mac, gotIP, wantIP, gotIP, mac, wantIP, wantIP)
}

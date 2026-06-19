// Package generateignition writes install-config.yaml and runs
// `openshift-install create single-node-ignition-config`. In manual-DNS
// bridge mode it also preflights that the required records resolve.
package generateignition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage renders install-config and produces the SNO ignition.
type Stage struct {
	installer interfaces.Installer
	dns       interfaces.DNSResolver
	host      interfaces.HostInspector
}

// New returns the generate-ignition stage.
func New(installer interfaces.Installer, dns interfaces.DNSResolver, host interfaces.HostInspector) *Stage {
	return &Stage{installer: installer, dns: dns, host: host}
}

func (*Stage) Name() string { return "generate-ignition" }

// Preflight validates the pull secret JSON and, in manual-DNS bridge mode,
// verifies the required records resolve to the master IP.
func (s *Stage) Preflight(ctx context.Context, sc *interfaces.StageContext) error {
	if err := config.ValidatePullSecretJSON(sc.Config.ConfigDir); err != nil {
		return err
	}
	if sc.Cluster.NetworkMode == config.NetworkModeBridge && sc.Cluster.DNSProvider == "" && sc.Cluster.MagicDNS == "" {
		// User manages DNS by hand — verify records resolve before burning
		// 30+ minutes on an unreachable-API install. When DNSProvider is set,
		// easyshift creates the records itself in the upsert-dns stage; when
		// MagicDNS is set, a wildcard service resolves the names with no
		// records to check.
		if err := s.host.LookPath("dig"); err != nil {
			return fmt.Errorf("DNS preflight needs `dig`: %w\n  hint: install bind-utils (Fedora/RHEL) or dnsutils (Debian/Ubuntu)", err)
		}
		if err := s.checkBridgeModeDNS(ctx, sc); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stage) Apply(ctx context.Context, sc *interfaces.StageContext) error {
	pullSecret, err := config.ReadPullSecret(sc.Config.ConfigDir)
	if err != nil {
		return err
	}
	pubKey, err := os.ReadFile(filepath.Join(sc.ClusterDir(), "id_rsa.pub"))
	if err != nil {
		return fmt.Errorf("read ssh public key: %w", err)
	}
	spec := sc.InstallerSpec()
	spec.PullSecret = pullSecret
	spec.SSHPublicKey = string(pubKey)
	if err := s.installer.WriteInstallConfig(ctx, spec); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		// vfkit attaches the Rosetta virtiofs device; drop the MachineConfig
		// that mounts it and registers the x86-64 binfmt handler so the
		// installed node runs amd64 images translated from first boot.
		if err := s.installer.WriteRosettaManifest(ctx, spec); err != nil {
			return err
		}
	}
	// Drop the baked-store MachineConfig before rendering ignition, so the
	// installed node mounts the store and CRI-O reads it from first boot. The
	// manifest must exist before CreateSingleNodeIgnition, which loads it.
	if sc.Cluster.BakeImages {
		if err := s.installer.WriteImageStoreManifest(ctx, spec); err != nil {
			return err
		}
	}
	return s.installer.CreateSingleNodeIgnition(ctx, spec)
}

func (*Stage) Rollback(_ context.Context, sc *interfaces.StageContext) error {
	for _, name := range []string{
		"install-config.yaml",
		"bootstrap-in-place-for-live-iso.ign",
		"master.ign",
		"worker.ign",
		"bootstrap.ign",
		"metadata.json",
		filepath.Join("openshift", "99-master-baked-image-store.yaml"),
	} {
		_ = os.Remove(filepath.Join(sc.ClusterDir(), name))
	}
	return nil
}

// checkBridgeModeDNS verifies every required record resolves to the master
// IP, aggregating all problems into one actionable error.
func (s *Stage) checkBridgeModeDNS(ctx context.Context, sc *interfaces.StageContext) error {
	fqdn := sc.Cluster.FQDN()
	ip := sc.Cluster.MasterIP

	var problems []string
	for _, name := range config.ClusterDNSNames(fqdn) {
		ips, err := s.dns.Resolve(ctx, name)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: lookup failed: %v", name, err))
		case len(ips) == 0:
			problems = append(problems, fmt.Sprintf("%s: no records", name))
		case !containsStr(ips, ip):
			problems = append(problems, fmt.Sprintf("%s: resolves to %v, want %s", name, ips, ip))
		}
	}
	if len(problems) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("bridge-mode DNS is not configured correctly:\n")
	for _, p := range problems {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	b.WriteString("create these records (all pointing at the master IP) on your DNS server, then retry:\n")
	fmt.Fprintf(&b, "    api.%s.\tA\t%s\n", fqdn, ip)
	fmt.Fprintf(&b, "    api-int.%s.\tA\t%s\n", fqdn, ip)
	fmt.Fprintf(&b, "    *.apps.%s.\tA\t%s\n", fqdn, ip)
	b.WriteString("optional but recommended (OpenShift docs): also add a PTR record so any node-discovery / cert-validation paths work; easyshift bakes the hostname so this is not strictly required:\n")
	fmt.Fprintf(&b, "    PTR for %s -> master-0.%s.\n", ip, fqdn)
	return errors.New(b.String())
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/csr"
	"github.com/TheEasyShift/easyshift/providers/dns"
	"github.com/TheEasyShift/easyshift/providers/exec"
	"github.com/TheEasyShift/easyshift/providers/fileserver"
	"github.com/TheEasyShift/easyshift/providers/host"
	"github.com/TheEasyShift/easyshift/providers/libvirt"
	"github.com/TheEasyShift/easyshift/providers/localca"
	"github.com/TheEasyShift/easyshift/providers/openshift"
	"github.com/TheEasyShift/easyshift/providers/redhat"
	"github.com/TheEasyShift/easyshift/providers/tls"
	"github.com/TheEasyShift/easyshift/providers/truststore"
	"github.com/TheEasyShift/easyshift/providers/vfkit"
	"github.com/TheEasyShift/easyshift/providers/vmnethelper"
)

// NewProductionDeps wires real implementations of every dependency for the
// host OS, rooted at cfg.ConfigDir. host is the host IP that VMs can reach
// (for file-server URLs). The compute + network providers differ by OS
// (libvirt on Linux, vfkit + vmnet-helper on macOS); everything else is shared.
func NewProductionDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {
	if runtime.GOOS == "darwin" {
		return NewDarwinDeps(cfg, hostIP)
	}
	return newLinuxDeps(cfg, hostIP)
}

// baseDeps builds the OS-independent dependency bag and returns the shared
// CommandRunner so the caller can construct the per-OS compute + network
// providers. VM and Net are left nil for the caller to fill in.
func baseDeps(cfg *config.Config, hostIP string) (interfaces.Deps, interfaces.CommandRunner, error) {
	cmd := exec.NewExecCommandRunner()
	dl := exec.NewHTTPDownloader()

	httpRoot := filepath.Join(cfg.ConfigDir, "http")
	files, err := fileserver.NewHTTPFileServer(httpRoot, hostIP, cfg.WebPort)
	if err != nil {
		return interfaces.Deps{}, nil, fmt.Errorf("init file server: %w", err)
	}

	return interfaces.Deps{
		Cmd:        cmd,
		Download:   dl,
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
	}, cmd, nil
}

// newLinuxDeps wires the libvirt-backed compute + networking.
func newLinuxDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {
	deps, cmd, err := baseDeps(cfg, hostIP)
	if err != nil {
		return interfaces.Deps{}, err
	}
	deps.VM = libvirt.NewLibvirtVMManager(cmd)
	deps.Net = libvirt.NewLibvirtNetworkProvisioner(cmd)
	return deps, nil
}

// NewDarwinDeps wires the macOS backend: vfkit compute + vmnet-helper
// networking. Everything else (installer, fileserver, CSR, DNS, ...) is shared
// with the Linux wiring via baseDeps.
func NewDarwinDeps(cfg *config.Config, hostIP string) (interfaces.Deps, error) {
	deps, cmd, err := baseDeps(cfg, hostIP)
	if err != nil {
		return interfaces.Deps{}, err
	}
	// The vmnet-helper provider is both the NetworkProvisioner and the per-VM
	// sidecar launcher the vfkit supervisor uses for guest networking.
	net := vmnethelper.NewNetworkProvisioner(cmd)
	deps.Net = net
	deps.VM = vfkit.NewVMManager(filepath.Join(cfg.ConfigDir, "vfkit"), net)
	return deps, nil
}

// newProductionDNSManager returns a libdns-backed DNSManager when a token is
// present, else a no-op (so the pipeline runs untouched when the user opts
// out of DNS automation; the per-stage DNSProvider check still gates calls).
func newProductionDNSManager(cfg *config.Config) interfaces.DNSManager {
	if _, err := os.Stat(config.DNSTokenPath(cfg.ConfigDir, config.DNSProviderCloudflare)); err == nil {
		if token, err := config.ReadDNSToken(cfg.ConfigDir, config.DNSProviderCloudflare); err == nil {
			if m, err := dns.NewManager(config.DNSProviderCloudflare, token); err == nil {
				return m
			}
		}
	}
	return nopDNSManager{}
}

// nopDNSManager is used when no DNS provider/token is configured.
type nopDNSManager struct{}

func (nopDNSManager) Upsert(_ context.Context, _, _, _ string) error { return nil }
func (nopDNSManager) Delete(_ context.Context, _, _ string) error    { return nil }

// Package fakes provides happy-path fake implementations of the easyshift
// side-effect interfaces. They record the calls they receive so tests can
// assert on them, and they return zero-value successful results so that
// integration-style tests can exercise the full ClusterManager flow without
// touching real resources.
package fakes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// CommandCall records a single CommandRunner invocation.
type CommandCall struct {
	Name      string
	Args      []string
	Streaming bool
}

// CommandRunner is a fake interfaces.CommandRunner. All commands succeed and
// produce empty output unless Output, Err, or RunFunc is set.
type CommandRunner struct {
	mu     sync.Mutex
	Calls  []CommandCall
	Output []byte
	Err    error
	// StreamStdout is written to the stdout writer of RunStreaming calls
	// (used to feed `openshift-install coreos print-stream-json`).
	StreamStdout []byte
	// RunFunc, if set, overrides Run's return (after the call is recorded),
	// letting a test fail specific invocations by inspecting name/args.
	RunFunc func(name string, args []string) ([]byte, error)
}

func (c *CommandRunner) record(call CommandCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, call)
}

// Run records the call and returns Output/Err. It also fakes the on-disk
// side effects of a small set of commands so downstream stages can read the
// expected files: ssh-keygen leaves an id_rsa{,.pub} pair at the path given
// after `-f`.
func (c *CommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c.record(CommandCall{Name: name, Args: append([]string(nil), args...)})
	if name == "ssh-keygen" {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-f" {
				_ = os.WriteFile(args[i+1], []byte("FAKE PRIVATE KEY"), 0o600)
				_ = os.WriteFile(args[i+1]+".pub", []byte("ssh-rsa AAAAFAKE test@fake"), 0o600)
				break
			}
		}
	}
	if c.RunFunc != nil {
		return c.RunFunc(name, args)
	}
	// Default node-Ready answer so simulate/full-pipeline tests converge
	// instantly (same spirit as the ssh-keygen side effect above). Tests that
	// need an un-Ready node set RunFunc.
	if len(c.Output) == 0 && c.Err == nil {
		for _, a := range args {
			if strings.Contains(a, `?(@.type=="Ready")`) {
				return []byte("True"), nil
			}
		}
	}
	return c.Output, c.Err
}

// RunStreaming records the call, writes StreamStdout to stdout, and returns Err.
func (c *CommandRunner) RunStreaming(_ context.Context, stdout, _ io.Writer, name string, args ...string) error {
	c.record(CommandCall{Name: name, Args: append([]string(nil), args...), Streaming: true})
	if len(c.StreamStdout) > 0 && stdout != nil {
		_, _ = stdout.Write(c.StreamStdout)
	}
	return c.Err
}

// DownloadCall records a single download.
type DownloadCall struct {
	URL      string
	DestPath string
}

// Downloader is a fake interfaces.Downloader. All downloads succeed and
// produce empty files unless the URL matches a special case below.
type Downloader struct {
	mu    sync.Mutex
	Calls []DownloadCall
	Err   error
	// ReleaseTxtBody overrides the canned release.txt body written by Download
	// for URLs ending in /release.txt. Empty means use the default ("4.99.0").
	ReleaseTxtBody string
}

// Download records the call and returns Err. It also writes a canned body
// for URLs ending in /release.txt so the OCP version resolver in
// ClusterManager.Create has a parseable input under test.
func (d *Downloader) Download(_ context.Context, url, destPath string) error {
	d.mu.Lock()
	d.Calls = append(d.Calls, DownloadCall{URL: url, DestPath: destPath})
	d.mu.Unlock()
	if d.Err != nil {
		return d.Err
	}
	// Simulate the download side effect so stages that read the file back work
	// (e.g. download-rhcos gunzipping the kernel). release.txt gets canned
	// channel content; everything else a small placeholder.
	body := []byte("FAKE DOWNLOAD")
	if strings.HasSuffix(url, "/release.txt") {
		s := "Name:      4.99.0\nDigest:    sha256:fake\nCreated:   2026-01-01T00:00:00Z\n"
		if d.ReleaseTxtBody != "" {
			s = d.ReleaseTxtBody
		}
		body = []byte(s)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destPath, body, 0o600)
}

// VMManager is a fake interfaces.VMManager. Created VMs are tracked in Created
// and considered running until Stop/Delete is called.
type VMManager struct {
	mu           sync.Mutex
	Created      []interfaces.VMSpec
	Started      []string
	Stopped      []string
	Deleted      []string
	ImportedISOs []string // volNames passed to ImportISO
	RemovedISOs  []string // volNames passed to RemoveISO
	running      map[string]bool
	Err          error
	// CheckAccessErr, if set, is returned by CheckAccess (simulates libvirt
	// being unreachable).
	CheckAccessErr error
	// StoragePoolErr, if set, is returned by StoragePoolActive (simulates a
	// missing or stopped storage pool).
	StoragePoolErr error
}

// Create records the spec.
func (v *VMManager) Create(_ context.Context, spec interfaces.VMSpec) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running == nil {
		v.running = map[string]bool{}
	}
	v.Created = append(v.Created, spec)
	v.running[spec.Name] = true
	return v.Err
}

// Start records the call.
func (v *VMManager) Start(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running == nil {
		v.running = map[string]bool{}
	}
	v.Started = append(v.Started, name)
	v.running[name] = true
	return v.Err
}

// Stop records the call.
func (v *VMManager) Stop(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Stopped = append(v.Stopped, name)
	delete(v.running, name)
	return v.Err
}

// Delete records the call.
func (v *VMManager) Delete(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Deleted = append(v.Deleted, name)
	delete(v.running, name)
	return v.Err
}

// IsRunning reports state tracked across Create/Start/Stop/Delete.
func (v *VMManager) IsRunning(_ context.Context, name string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running[name], v.Err
}

// SetRunning marks a VM running (or not) for IsRunning without recording a
// Start/Stop call. Test helper.
func (v *VMManager) SetRunning(name string, running bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running == nil {
		v.running = map[string]bool{}
	}
	v.running[name] = running
}

// ImportISO records volName and returns a deterministic fake pool path.
func (v *VMManager) ImportISO(_ context.Context, _, volName, _ string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ImportedISOs = append(v.ImportedISOs, volName)
	if v.Err != nil {
		return "", v.Err
	}
	return "/var/lib/libvirt/images/" + volName, nil
}

// RemoveISO records volName.
func (v *VMManager) RemoveISO(_ context.Context, _, volName string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.RemovedISOs = append(v.RemovedISOs, volName)
	return v.Err
}

// CheckAccess returns CheckAccessErr (nil by default).
func (v *VMManager) CheckAccess(_ context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.CheckAccessErr
}

// StoragePoolActive returns StoragePoolErr (nil by default).
func (v *VMManager) StoragePoolActive(_ context.Context, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.StoragePoolErr
}

// NetworkProvisioner is a fake interfaces.NetworkProvisioner.
type NetworkProvisioner struct {
	mu      sync.Mutex
	Ensured []interfaces.NetworkSpec // EnsureNetwork calls
	Added   []HostCall               // AddHost calls
	Removed []HostCall               // RemoveHost calls
	// Info is what InspectNetwork returns; tests seed it to model the live
	// network. ResetCalls records the networks passed to ResetNetwork.
	Info       interfaces.NetworkInfo
	ResetCalls []string
	Err        error
}

// HostCall records one AddHost/RemoveHost invocation.
type HostCall struct {
	Network string
	Host    interfaces.DHCPHost
}

// EnsureNetwork records the spec.
func (n *NetworkProvisioner) EnsureNetwork(_ context.Context, spec interfaces.NetworkSpec) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Ensured = append(n.Ensured, spec)
	return n.Err
}

// AddHost records the reservation.
func (n *NetworkProvisioner) AddHost(_ context.Context, network string, host interfaces.DHCPHost) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Added = append(n.Added, HostCall{Network: network, Host: host})
	return n.Err
}

// RemoveHost records the reservation removal.
func (n *NetworkProvisioner) RemoveHost(_ context.Context, network string, host interfaces.DHCPHost) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Removed = append(n.Removed, HostCall{Network: network, Host: host})
	return n.Err
}

// InspectNetwork returns the seeded Info snapshot.
func (n *NetworkProvisioner) InspectNetwork(_ context.Context, _ string) (interfaces.NetworkInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Info, n.Err
}

// ResetNetwork records the call and marks the seeded network as gone.
func (n *NetworkProvisioner) ResetNetwork(_ context.Context, network string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ResetCalls = append(n.ResetCalls, network)
	n.Info = interfaces.NetworkInfo{Exists: false}
	return n.Err
}

// Installer is a fake interfaces.Installer. LastSpec captures the spec from
// the most recent method call so tests can assert on the resolved binary
// paths the stages produced.
type Installer struct {
	mu                   sync.Mutex
	WroteInstallConfig   bool
	CreatedIgnitions     bool
	CreatedSingleNodeIgn bool
	EmbeddedISO          bool
	EmbeddedNetwork      bool
	// LastNetworkKeyfile is the keyfile path passed to the most recent
	// EmbedNetworkKeyfileInISO call (empty if never called).
	LastNetworkKeyfile  string
	WaitedForInstall    bool
	WaitForInstallCalls int
	// WaitForInstallTimeouts, if > 0, causes WaitForInstallComplete to fail
	// with a synthetic "exit status 6" error that many times before
	// succeeding. Used to exercise easyshift's retry loop.
	WaitForInstallTimeouts int
	LastSpec               interfaces.InstallerSpec
	// LiveISOURL overrides the URL returned by CoreOSLiveISOURL.
	LiveISOURL string
	Err        error
}

func (i *Installer) record(spec interfaces.InstallerSpec) {
	i.LastSpec = spec
}

func (i *Installer) WriteInstallConfig(_ context.Context, spec interfaces.InstallerSpec) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.WroteInstallConfig = true
	i.record(spec)
	return i.Err
}

func (i *Installer) CreateIgnitionConfigs(_ context.Context, spec interfaces.InstallerSpec) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.CreatedIgnitions = true
	i.record(spec)
	return i.Err
}

func (i *Installer) CreateSingleNodeIgnition(_ context.Context, spec interfaces.InstallerSpec) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.CreatedSingleNodeIgn = true
	i.record(spec)
	if i.Err != nil {
		return i.Err
	}
	// Write the SNO ignition the real installer would produce, so downstream
	// stages (embed-ignition-iso / publish-pxe-assets) can read it.
	if spec.ClusterDir != "" {
		if err := os.MkdirAll(spec.ClusterDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(spec.ClusterDir, "bootstrap-in-place-for-live-iso.ign"), []byte(`{"ignition":{"version":"3.4.0"}}`), 0o600)
	}
	return nil
}

func (i *Installer) EmbedIgnitionInISO(_ context.Context, spec interfaces.InstallerSpec, _, _, _ string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.EmbeddedISO = true
	i.record(spec)
	return i.Err
}

func (i *Installer) EmbedNetworkKeyfileInISO(_ context.Context, spec interfaces.InstallerSpec, keyfilePath, _ string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.EmbeddedNetwork = true
	i.LastNetworkKeyfile = keyfilePath
	i.record(spec)
	return i.Err
}

func (i *Installer) WaitForInstallComplete(_ context.Context, spec interfaces.InstallerSpec) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.WaitedForInstall = true
	i.WaitForInstallCalls++
	i.record(spec)
	// WaitForInstallTimeouts simulates openshift-install exiting with status 6
	// for the first N calls before succeeding — used to test the retry loop.
	if i.WaitForInstallTimeouts > 0 {
		i.WaitForInstallTimeouts--
		return errors.New("command openshift-install failed: exit status 6")
	}
	return i.Err
}

// CoreOSLiveISOURL returns a canned URL (overridable via LiveISOURL).
func (i *Installer) CoreOSLiveISOURL(_ context.Context, spec interfaces.InstallerSpec) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.record(spec)
	if i.Err != nil {
		return "", i.Err
	}
	if i.LiveISOURL != "" {
		return i.LiveISOURL, nil
	}
	return "https://rhcos.example.com/rhcos-live.x86_64.iso", nil
}

// CoreOSLivePXEURLs returns canned PXE asset URLs.
func (i *Installer) CoreOSLivePXEURLs(_ context.Context, spec interfaces.InstallerSpec) (interfaces.CoreOSLivePXE, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.record(spec)
	if i.Err != nil {
		return interfaces.CoreOSLivePXE{}, i.Err
	}
	return interfaces.CoreOSLivePXE{
		KernelURL:    "https://rhcos.example.com/live-kernel.aarch64",
		InitramfsURL: "https://rhcos.example.com/live-initramfs.aarch64.img",
		RootfsURL:    "https://rhcos.example.com/live-rootfs.aarch64.img",
	}, nil
}

// FileServer is a fake interfaces.FileServer.
type FileServer struct {
	mu      sync.Mutex
	Root    string
	URL     string
	started bool
	stopped bool
	Err     error
}

func (f *FileServer) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return f.Err
}

func (f *FileServer) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return f.Err
}

func (f *FileServer) RootDir() string { return f.Root }
func (f *FileServer) BaseURL() string { return f.URL }

// Started reports whether Start has been called.
func (f *FileServer) Started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// Stopped reports whether Stop has been called.
func (f *FileServer) Stopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// CSRApprover is a fake interfaces.CSRApprover. Run records that it was
// started and blocks until ctx is cancelled, mirroring the real loop's
// shutdown semantics.
type CSRApprover struct {
	mu         sync.Mutex
	Started    bool
	LastOCPath string
	Err        error
}

// Run records the call and blocks until ctx is done.
func (a *CSRApprover) Run(ctx context.Context, ocPath, _ string) error {
	a.mu.Lock()
	a.Started = true
	a.LastOCPath = ocPath
	a.mu.Unlock()
	<-ctx.Done()
	return a.Err
}

// WasStarted reports whether Run has been called.
func (a *CSRApprover) WasStarted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Started
}

// CertIssuer is a fake interfaces.CertIssuer. It records the domain sets
// it was asked to issue for, and returns a canned PEM cert+key so the
// downstream `oc apply` calls have plausible inputs.
type CertIssuer struct {
	mu       sync.Mutex
	Issued   [][]string // each entry is the domains list passed to Issue
	LastOpts interfaces.CertIssuerOpts
	Err      error
}

func (c *CertIssuer) Issue(_ context.Context, domains []string) ([]byte, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Issued = append(c.Issued, append([]string(nil), domains...))
	if c.Err != nil {
		return nil, nil, c.Err
	}
	return []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"),
		[]byte("-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n"),
		nil
}

// recordCertIssuerOpts is exposed so the All() factory can wire a
// closure that captures the most recent NewCertIssuer call's opts.
func (c *CertIssuer) recordOpts(o interfaces.CertIssuerOpts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastOpts = o
}

// DNSManager is a fake interfaces.DNSManager. Records every Upsert/Delete
// so tests can assert easyshift drove the correct provider calls.
type DNSManager struct {
	mu      sync.Mutex
	Upserts []DNSCall
	Deletes []DNSCall
	Err     error
}

// DNSCall captures one Upsert or Delete invocation.
type DNSCall struct {
	Zone string
	FQDN string
	IP   string // empty for Delete
}

func (d *DNSManager) Upsert(_ context.Context, zone, fqdn, ip string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Upserts = append(d.Upserts, DNSCall{Zone: zone, FQDN: fqdn, IP: ip})
	return d.Err
}

func (d *DNSManager) Delete(_ context.Context, zone, fqdn string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Deletes = append(d.Deletes, DNSCall{Zone: zone, FQDN: fqdn})
	return d.Err
}

// HostnameInjector is a fake interfaces.HostnameInjector. Run records the
// target hostname and blocks until ctx is cancelled, mirroring the real
// loop's shutdown semantics.
type HostnameInjector struct {
	mu           sync.Mutex
	Started      bool
	LastIP       string
	LastHostname string
	LastKeyPath  string
	Err          error
}

func (h *HostnameInjector) Run(ctx context.Context, ip, sshKeyPath, hostname string) error {
	h.mu.Lock()
	h.Started = true
	h.LastIP = ip
	h.LastHostname = hostname
	h.LastKeyPath = sshKeyPath
	h.mu.Unlock()
	<-ctx.Done()
	return h.Err
}

func (h *HostnameInjector) WasStarted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Started
}

// HostInspector is a fake interfaces.HostInspector. By default every
// preflight check passes. Tests can flip per-check fields to simulate a
// missing binary, an interface that doesn't exist, no CPU virtualization,
// or insufficient disk space.
type HostInspector struct {
	mu sync.Mutex
	// NoVirtualization makes HasCPUVirtualization return false.
	NoVirtualization bool
	// MissingBridges names bridges that should report as not existing
	// (InspectBridge returns BridgeInfo{Exists:false}). Convenience knob for
	// the common "user passed a name that isn't a bridge" case.
	MissingBridges map[string]bool
	// Bridges overrides InspectBridge for the named bridges. A name with no
	// entry (and not in MissingBridges) is treated as a healthy bridge:
	// exists, one slave "eth0", up. Use this knob to model partially-broken
	// bridges (e.g. exists but no slaves, or down).
	Bridges map[string]interfaces.BridgeInfo
	// MissingBinaries names binaries that LookPath should fail for.
	MissingBinaries map[string]bool
	// DiskAvailable, if non-zero, overrides AvailableDiskBytes. The default
	// (0) is interpreted as "effectively infinite" so tests pass without
	// configuring a value.
	DiskAvailable uint64
	// ARPTable maps MAC -> IP; lookups miss return "".
	ARPTable map[string]string
	// TCPReachable maps addr -> nil (reachable) or error (unreachable).
	// Missing entries default to reachable.
	TCPReachable map[string]error
	// Err, if set, is returned from every method.
	Err error
}

func (h *HostInspector) HasCPUVirtualization() (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return false, h.Err
	}
	return !h.NoVirtualization, nil
}

func (h *HostInspector) InspectBridge(name string) (interfaces.BridgeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return interfaces.BridgeInfo{}, h.Err
	}
	if h.MissingBridges[name] {
		return interfaces.BridgeInfo{Exists: false}, nil
	}
	if info, ok := h.Bridges[name]; ok {
		return info, nil
	}
	return interfaces.BridgeInfo{Exists: true, Slaves: []string{"eth0"}, Up: true}, nil
}

func (h *HostInspector) LookPath(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return h.Err
	}
	if h.MissingBinaries[name] {
		return errors.New("not found: " + name)
	}
	return nil
}

func (h *HostInspector) AvailableDiskBytes(_ string) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return 0, h.Err
	}
	if h.DiskAvailable == 0 {
		return 1 << 62, nil
	}
	return h.DiskAvailable, nil
}

func (h *HostInspector) ARPLookup(mac string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return "", h.Err
	}
	return h.ARPTable[strings.ToLower(mac)], nil
}

func (h *HostInspector) DialTCP(addr string, _ time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Err != nil {
		return h.Err
	}
	if err, ok := h.TCPReachable[addr]; ok {
		return err
	}
	return nil
}

// DNSResolver is a fake interfaces.DNSResolver. Records maps name → IPs;
// unknown names resolve to nil (empty result), matching what `dig +short`
// does for a missing record.
type DNSResolver struct {
	mu      sync.Mutex
	Records map[string][]string
	Err     error
}

// Resolve returns the configured records for name, or nil if unset.
func (r *DNSResolver) Resolve(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return nil, r.Err
	}
	return r.Records[name], nil
}

// PullSecretFetcher is a fake interfaces.PullSecretFetcher. It hands back a
// canned prompt and a syntactically valid pull secret unless errors are set.
type PullSecretFetcher struct {
	StartErr error
	FetchErr error
	// Secret overrides the default fake pull secret returned by WaitAndFetch.
	Secret      []byte
	StartCalled bool
	FetchCalled bool
}

// StartDeviceAuth records the call and returns a canned prompt or StartErr.
func (p *PullSecretFetcher) StartDeviceAuth(_ context.Context) (interfaces.DeviceAuthPrompt, error) {
	p.StartCalled = true
	if p.StartErr != nil {
		return interfaces.DeviceAuthPrompt{}, p.StartErr
	}
	return interfaces.DeviceAuthPrompt{VerificationURI: "https://fake.sso.example/device", UserCode: "FAKE-CODE"}, nil
}

// WaitAndFetch records the call and returns Secret (or a valid stand-in) or FetchErr.
func (p *PullSecretFetcher) WaitAndFetch(_ context.Context) ([]byte, error) {
	p.FetchCalled = true
	if p.FetchErr != nil {
		return nil, p.FetchErr
	}
	if len(p.Secret) == 0 {
		return []byte(`{"auths":{"fake.registry":{"auth":"ZmFrZQ=="}}}`), nil
	}
	return p.Secret, nil
}

// TrustStore is a fake interfaces.TrustStoreInstaller recording calls.
type TrustStore struct {
	mu          sync.Mutex
	Installed   []string // caCertPath per Install call
	Uninstalled []string // caCertPath per Uninstall call
	Err         error
}

func (t *TrustStore) Install(_ context.Context, caCertPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Installed = append(t.Installed, caCertPath)
	return t.Err
}

func (t *TrustStore) Uninstall(_ context.Context, caCertPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Uninstalled = append(t.Uninstalled, caCertPath)
	return t.Err
}

// fakeHTTPRoot returns a writable temp dir so stages that publish files into
// the fileserver root (e.g. publish-pxe-assets) work under the fakes.
func fakeHTTPRoot() string {
	d, err := os.MkdirTemp("", "easyshift-fake-http-")
	if err != nil {
		return os.TempDir()
	}
	return d
}

// All returns a Deps wired with one fresh fake per interface, ready for tests
// that want a vanilla happy-path environment.
func All() (interfaces.Deps, *Bundle) {
	b := &Bundle{
		Cmd:             &CommandRunner{},
		Download:        &Downloader{},
		VM:              &VMManager{},
		Net:             &NetworkProvisioner{},
		Installer:       &Installer{},
		Files:           &FileServer{Root: fakeHTTPRoot(), URL: "http://fake:9393"},
		CSR:             &CSRApprover{},
		Hostname:        &HostnameInjector{},
		Host:            &HostInspector{},
		DNS:             &DNSResolver{},
		DNSManager:      &DNSManager{},
		CertIssuer:      &CertIssuer{},
		LocalCertIssuer: &CertIssuer{},
		PullSecret:      &PullSecretFetcher{},
		TrustStore:      &TrustStore{},
	}
	return interfaces.Deps{
		Cmd:        b.Cmd,
		Download:   b.Download,
		VM:         b.VM,
		Net:        b.Net,
		Installer:  b.Installer,
		Files:      b.Files,
		CSR:        b.CSR,
		Hostname:   b.Hostname,
		Host:       b.Host,
		DNS:        b.DNS,
		DNSManager: b.DNSManager,
		PullSecret: b.PullSecret,
		TrustStore: b.TrustStore,
		NewCertIssuer: func(opts interfaces.CertIssuerOpts) (interfaces.CertIssuer, error) {
			b.CertIssuer.recordOpts(opts)
			return b.CertIssuer, nil
		},
		NewLocalCertIssuer: func(_ string) (interfaces.CertIssuer, error) {
			return b.LocalCertIssuer, nil
		},
	}, b
}

// WriteTrace prints a human-readable summary of every operation the
// real pipeline would have performed. Used by `easyshift --simulate` to
// give the operator a complete inspection point without touching real
// libvirt/DNS/ACME.
func (b *Bundle) WriteTrace(w io.Writer) {
	fmt.Fprintln(w, "=== simulation trace ===")

	if len(b.VM.Created) > 0 {
		fmt.Fprintf(w, "\nVMs created (%d):\n", len(b.VM.Created))
		for _, v := range b.VM.Created {
			fmt.Fprintf(w, "  %s  ram=%dMiB  vcpus=%d  disk=%dGiB  net=%q  boot-iso=%q\n",
				v.Name, v.MemoryMiB, v.VCPUs, v.DiskSizeGiB, v.NetworkArg, v.BootISO)
		}
	}
	if len(b.VM.Started) > 0 {
		fmt.Fprintf(w, "\nVMs started: %v\n", b.VM.Started)
	}
	if len(b.VM.Stopped) > 0 {
		fmt.Fprintf(w, "\nVMs stopped: %v\n", b.VM.Stopped)
	}
	if len(b.VM.Deleted) > 0 {
		fmt.Fprintf(w, "\nVMs deleted: %v\n", b.VM.Deleted)
	}
	if len(b.VM.ImportedISOs) > 0 {
		fmt.Fprintf(w, "\nISOs imported to libvirt pool: %v\n", b.VM.ImportedISOs)
	}

	if len(b.Net.Ensured) > 0 {
		fmt.Fprintf(w, "\nShared NAT network ensured (%d):\n", len(b.Net.Ensured))
		for _, n := range b.Net.Ensured {
			fmt.Fprintf(w, "  name=%s subnet=%s domain=%s\n", n.Name, n.Subnet, n.Domain)
		}
	}
	if len(b.Net.Added) > 0 {
		fmt.Fprintf(w, "\nDHCP reservations added (%d):\n", len(b.Net.Added))
		for _, h := range b.Net.Added {
			fmt.Fprintf(w, "  net=%s mac=%s ip=%s host=%s\n", h.Network, h.Host.MAC, h.Host.IP, h.Host.Hostname)
		}
	}

	if len(b.Download.Calls) > 0 {
		fmt.Fprintf(w, "\nDownloads (%d):\n", len(b.Download.Calls))
		for _, d := range b.Download.Calls {
			fmt.Fprintf(w, "  %s\n     -> %s\n", d.URL, d.DestPath)
		}
	}

	if len(b.DNSManager.Upserts) > 0 {
		fmt.Fprintf(w, "\nDNS records upserted (%d):\n", len(b.DNSManager.Upserts))
		for _, c := range b.DNSManager.Upserts {
			fmt.Fprintf(w, "  zone=%s  fqdn=%s  ip=%s   (api.<fqdn>, api-int.<fqdn>, *.apps.<fqdn>)\n",
				c.Zone, c.FQDN, c.IP)
		}
	}
	if len(b.DNSManager.Deletes) > 0 {
		fmt.Fprintf(w, "\nDNS records deleted (%d):\n", len(b.DNSManager.Deletes))
		for _, c := range b.DNSManager.Deletes {
			fmt.Fprintf(w, "  zone=%s  fqdn=%s\n", c.Zone, c.FQDN)
		}
	}

	if len(b.CertIssuer.Issued) > 0 {
		fmt.Fprintf(w, "\nTLS certs requested (%d):\n", len(b.CertIssuer.Issued))
		for _, d := range b.CertIssuer.Issued {
			fmt.Fprintf(w, "  %v\n", d)
		}
		if b.CertIssuer.LastOpts.Email != "" {
			fmt.Fprintf(w, "  ACME account: email=%s  staging=%t  provider=%s\n",
				b.CertIssuer.LastOpts.Email, b.CertIssuer.LastOpts.Staging, b.CertIssuer.LastOpts.DNSProvider)
		}
	}
	if len(b.LocalCertIssuer.Issued) > 0 {
		fmt.Fprintf(w, "\nLocal-CA TLS certs issued (%d):\n", len(b.LocalCertIssuer.Issued))
		for _, d := range b.LocalCertIssuer.Issued {
			fmt.Fprintf(w, "  %v\n", d)
		}
	}

	if b.Installer.WroteInstallConfig || b.Installer.CreatedSingleNodeIgn ||
		b.Installer.EmbeddedISO || b.Installer.WaitedForInstall {
		fmt.Fprintf(w, "\nInstaller methods invoked:\n")
		if b.Installer.WroteInstallConfig {
			fmt.Fprintln(w, "  - WriteInstallConfig")
		}
		if b.Installer.CreatedSingleNodeIgn {
			fmt.Fprintln(w, "  - CreateSingleNodeIgnition")
		}
		if b.Installer.EmbeddedISO {
			fmt.Fprintln(w, "  - EmbedIgnitionInISO")
		}
		if b.Installer.EmbeddedNetwork {
			fmt.Fprintln(w, "  - EmbedNetworkKeyfileInISO (static master IP)")
		}
		if b.Installer.WaitedForInstall {
			fmt.Fprintf(w, "  - WaitForInstallComplete (calls=%d)\n", b.Installer.WaitForInstallCalls)
		}
	}

	if b.CSR.WasStarted() {
		fmt.Fprintln(w, "\nCSR approver: launched (oc path:", b.CSR.LastOCPath+")")
	}
	if b.Hostname.WasStarted() {
		fmt.Fprintf(w, "\nHostname injector: launched (target=%s)\n", b.Hostname.LastHostname)
	}

	if len(b.TrustStore.Installed)+len(b.TrustStore.Uninstalled) > 0 {
		fmt.Fprintf(w, "\nTrust store: installs=%v uninstalls=%v\n",
			b.TrustStore.Installed, b.TrustStore.Uninstalled)
	}

	if len(b.Cmd.Calls) > 0 {
		fmt.Fprintf(w, "\nShell commands run (%d):\n", len(b.Cmd.Calls))
		for _, c := range b.Cmd.Calls {
			marker := " "
			if c.Streaming {
				marker = "*"
			}
			fmt.Fprintf(w, "  %s %s %s\n", marker, c.Name, strings.Join(c.Args, " "))
		}
		fmt.Fprintln(w, "  (* = RunStreaming; output piped to terminal+log)")
	}
}

// Bundle groups the concrete fakes returned by All so tests can read recorded
// calls without re-asserting interface types.
type Bundle struct {
	Cmd             *CommandRunner
	Download        *Downloader
	VM              *VMManager
	Net             *NetworkProvisioner
	Installer       *Installer
	Files           *FileServer
	CSR             *CSRApprover
	Hostname        *HostnameInjector
	Host            *HostInspector
	DNS             *DNSResolver
	DNSManager      *DNSManager
	CertIssuer      *CertIssuer
	LocalCertIssuer *CertIssuer
	PullSecret      *PullSecretFetcher
	TrustStore      *TrustStore
}

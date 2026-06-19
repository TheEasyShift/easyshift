package libvirt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// LibvirtSystemURI is the libvirt connection URI easyshift targets for every
// VM and network operation.
const LibvirtSystemURI = "qemu:///system"

const (
	virshCmd       = "virsh"
	virtInstallCmd = "virt-install"
	defaultCPUMode = "host-passthrough"
	// defaultOSInfo lets modern virt-install (which mandates an OS hint)
	// detect RHCOS from the boot media and fall back to generic defaults
	// rather than erroring when detection can't pin an exact osinfo name.
	defaultOSInfo = "detect=on,require=off"
)

// LibvirtVMManager implements VMManager via virsh and virt-install.
type LibvirtVMManager struct {
	cmd interfaces.CommandRunner
}

// NewLibvirtVMManager returns a VMManager backed by libvirt CLI tools.
func NewLibvirtVMManager(cmd interfaces.CommandRunner) *LibvirtVMManager {
	return &LibvirtVMManager{cmd: cmd}
}

// virsh runs a virsh subcommand against the system libvirt instance. Every
// operation must name qemu:///system explicitly: bare virsh resolves the
// connection from the ambient default URI, which for a non-root user with no
// LIBVIRT_DEFAULT_URI is qemu:///session — and that unprivileged daemon can't
// create the NAT bridge ("Operation not permitted"). The CheckAccess preflight
// already targets system, so without this the preflight would pass while the
// real work silently went to the session daemon and failed.
func (m *LibvirtVMManager) virsh(ctx context.Context, args ...string) ([]byte, error) {
	return m.cmd.Run(ctx, virshCmd, append([]string{"-c", LibvirtSystemURI}, args...)...)
}

// Create defines a new VM via `virt-install`. If spec.BootISO is set the VM
// boots from that ISO (SNO bootstrap-in-place); otherwise it PXE-boots and
// uses spec.KernelArgs.
func (m *LibvirtVMManager) Create(ctx context.Context, spec interfaces.VMSpec) error {
	args := []string{
		"--connect", LibvirtSystemURI,
		"--name", spec.Name,
		"--memory", strconv.Itoa(spec.MemoryMiB),
		"--vcpus", strconv.Itoa(spec.VCPUs),
		"--cpu", defaultCPUMode,
		"--osinfo", defaultOSInfo,
		"--disk", fmt.Sprintf("pool=%s,size=%d,bus=virtio", spec.StoragePool, spec.DiskSizeGiB),
		"--network", spec.NetworkArg,
		// SNO bootstrap-in-place writes RHCOS to disk and reboots to pivot off
		// the live ISO. Order hd first so the post-install reboot lands on the
		// installed OS; the empty disk falls through to the ISO on first boot,
		// matching the OCP-prescribed boot order ("default to booting from the
		// target installation disk" with the ISO booted once for discovery).
		"--boot", "hd,cdrom",
		"--noautoconsole",
	}
	if spec.BootISO != "" {
		args = append(args, "--cdrom", spec.BootISO)
	} else {
		args = append(args, "--pxe")
		if spec.KernelArgs != "" {
			args = append(args, "--extra-args", spec.KernelArgs)
		}
	}
	if _, err := m.cmd.Run(ctx, virtInstallCmd, args...); err != nil {
		return fmt.Errorf("virt-install %s: %w", spec.Name, err)
	}
	return nil
}

// Start boots a VM and waits up to 60s for it to enter the running state.
func (m *LibvirtVMManager) Start(ctx context.Context, name string) error {
	if _, err := m.virsh(ctx, "start", name); err != nil {
		return fmt.Errorf("virsh start %s: %w", name, err)
	}
	for i := 0; i < 30; i++ {
		running, err := m.IsRunning(ctx, name)
		if err == nil && running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for VM %s to start", name)
}

// Stop gracefully shuts down a VM, falling back to forced destroy after 60s.
func (m *LibvirtVMManager) Stop(ctx context.Context, name string) error {
	if _, err := m.virsh(ctx, "shutdown", name); err != nil {
		return fmt.Errorf("virsh shutdown %s: %w", name, err)
	}
	for i := 0; i < 30; i++ {
		running, err := m.IsRunning(ctx, name)
		if err == nil && !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if _, err := m.virsh(ctx, "destroy", name); err != nil {
		return fmt.Errorf("virsh destroy %s: %w", name, err)
	}
	return nil
}

// Delete undefines a VM and removes its storage.
func (m *LibvirtVMManager) Delete(ctx context.Context, name string) error {
	running, _ := m.IsRunning(ctx, name)
	if running {
		if err := m.Stop(ctx, name); err != nil {
			return err
		}
	}
	if _, err := m.virsh(ctx, "undefine", name, "--remove-all-storage"); err != nil {
		return fmt.Errorf("virsh undefine %s: %w", name, err)
	}
	return nil
}

// IsRunning returns true if `virsh domstate` reports the VM as running.
func (m *LibvirtVMManager) IsRunning(ctx context.Context, name string) (bool, error) {
	out, err := m.virsh(ctx, "domstate", name)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "running", nil
}

// ImportISO uploads localPath into the default storage pool as a raw volume
// named volName and returns the resulting pool volume path. The upload
// streams bytes through libvirtd (which reads them from the client), so the
// source file only needs to be readable by the easyshift process — it does
// not need to be reachable by the qemu user.
func (m *LibvirtVMManager) ImportISO(ctx context.Context, pool, volName, localPath string) (string, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("stat iso %s: %w", localPath, err)
	}
	// Drop any stale volume from a previous attempt so vol-create-as is idempotent.
	_, _ = m.virsh(ctx, "vol-delete", "--pool", pool, volName)

	if _, err := m.virsh(ctx, "vol-create-as", pool, volName,
		strconv.FormatInt(fi.Size(), 10), "--format", "raw"); err != nil {
		return "", fmt.Errorf("vol-create-as %s: %w", volName, err)
	}
	if _, err := m.virsh(ctx, "vol-upload", "--pool", pool, volName, localPath); err != nil {
		return "", fmt.Errorf("vol-upload %s: %w", volName, err)
	}
	out, err := m.virsh(ctx, "vol-path", "--pool", pool, volName)
	if err != nil {
		return "", fmt.Errorf("vol-path %s: %w", volName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoveISO deletes a volume created by ImportISO. Missing volumes are not
// an error (best-effort cleanup during rollback).
func (m *LibvirtVMManager) RemoveISO(ctx context.Context, pool, volName string) error {
	_, _ = m.virsh(ctx, "vol-delete", "--pool", pool, volName)
	return nil
}

// CheckAccess runs a cheap probe against qemu:///system to detect the common
// deploy-time problems: libvirtd not running, user not in the libvirt group,
// or a polkit denial.
func (m *LibvirtVMManager) CheckAccess(ctx context.Context) error {
	if _, err := m.virsh(ctx, "list", "--name"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("`virsh` not found on PATH: %w\n  hint: install the libvirt client tools (libvirt-client on Fedora/RHEL, libvirt-clients on Debian/Ubuntu)", err)
		}
		return fmt.Errorf("libvirt at %s is not reachable: %w\n  hint: ensure libvirtd/virtqemud is running and your user is in the 'libvirt' group", LibvirtSystemURI, err)
	}
	return nil
}

// StoragePoolActive verifies the named libvirt pool exists and is running,
// since both the master disk and the boot ISO are created there.
func (m *LibvirtVMManager) StoragePoolActive(ctx context.Context, pool string) error {
	out, err := m.virsh(ctx, "pool-info", "--pool", pool)
	if err != nil {
		return fmt.Errorf("libvirt storage pool %q not found: %w\n  hint: pass --storage-pool <name> to use an existing pool (see `virsh pool-list --all`), or create it: virsh pool-define-as %s dir --target /var/lib/libvirt/images && virsh pool-autostart %s && virsh pool-start %s",
			pool, err, pool, pool, pool)
	}
	if !strings.Contains(string(out), "running") {
		return fmt.Errorf("libvirt storage pool %q exists but is not running\n  hint: virsh pool-start %s", pool, pool)
	}
	return nil
}

// LibvirtNetworkProvisioner implements NetworkProvisioner for libvirt NAT networks.
type LibvirtNetworkProvisioner struct {
	cmd interfaces.CommandRunner
}

// NewLibvirtNetworkProvisioner returns a NetworkProvisioner backed by virsh.
func NewLibvirtNetworkProvisioner(cmd interfaces.CommandRunner) *LibvirtNetworkProvisioner {
	return &LibvirtNetworkProvisioner{cmd: cmd}
}

// virsh runs a virsh subcommand against the system libvirt instance. See the
// LibvirtVMManager.virsh note: NAT bridge creation fails with "Operation not
// permitted" if these calls fall back to the unprivileged qemu:///session.
func (p *LibvirtNetworkProvisioner) virsh(ctx context.Context, args ...string) ([]byte, error) {
	return p.cmd.Run(ctx, virshCmd, append([]string{"-c", LibvirtSystemURI}, args...)...)
}

// EnsureNetwork creates the shared NAT network if it doesn't already exist,
// and ensures it's running. Idempotent: every NAT cluster calls it, but only
// the first actually defines the network. Reservations are added separately
// via AddHost (not baked into the definition) so adding/removing a cluster
// doesn't churn the shared network.
func (p *LibvirtNetworkProvisioner) EnsureNetwork(ctx context.Context, spec interfaces.NetworkSpec) error {
	if _, err := p.virsh(ctx, "net-info", spec.Name); err == nil {
		// Already defined — make sure it's running (no-op if it already is).
		_, _ = p.virsh(ctx, "net-start", spec.Name)
		return nil
	}

	xmlFile, err := writeTempFile(spec.Name+"-network-*.xml", []byte(buildNetworkXML(spec)))
	if err != nil {
		return fmt.Errorf("write network XML: %w", err)
	}
	defer func() { _ = os.Remove(xmlFile) }()

	if _, err := p.virsh(ctx, "net-define", xmlFile); err != nil {
		return fmt.Errorf("net-define %s: %w", spec.Name, err)
	}
	// Now defined. If a later step fails, undefine so a retry isn't blocked by
	// a defined-but-inactive network (the runner won't roll back a stage whose
	// Apply returned an error, so we self-clean here).
	if _, err := p.virsh(ctx, "net-start", spec.Name); err != nil {
		_, _ = p.virsh(ctx, "net-undefine", spec.Name)
		return fmt.Errorf("net-start %s: %w", spec.Name, err)
	}
	if _, err := p.virsh(ctx, "net-autostart", spec.Name); err != nil {
		_, _ = p.virsh(ctx, "net-destroy", spec.Name)
		_, _ = p.virsh(ctx, "net-undefine", spec.Name)
		return fmt.Errorf("net-autostart %s: %w", spec.Name, err)
	}
	return nil
}

// AddHost adds a DHCP reservation to the live network and its persistent
// config. Idempotent: an already-present reservation is treated as success.
func (p *LibvirtNetworkProvisioner) AddHost(ctx context.Context, network string, host interfaces.DHCPHost) error {
	entry := fmt.Sprintf("<host mac='%s' name='%s' ip='%s'/>", host.MAC, host.Hostname, host.IP)
	out, err := p.virsh(ctx, "net-update", network, "add", "ip-dhcp-host", entry, "--live", "--config")
	if err != nil && !strings.Contains(string(out), "already") && !strings.Contains(err.Error(), "already") {
		return fmt.Errorf("net-update add host %s on %s: %w", host.MAC, network, err)
	}
	return nil
}

// RemoveHost removes a DHCP reservation. Idempotent: a missing reservation is
// treated as success (best-effort cleanup on cluster delete).
func (p *LibvirtNetworkProvisioner) RemoveHost(ctx context.Context, network string, host interfaces.DHCPHost) error {
	entry := fmt.Sprintf("<host mac='%s' name='%s' ip='%s'/>", host.MAC, host.Hostname, host.IP)
	out, err := p.virsh(ctx, "net-update", network, "delete", "ip-dhcp-host", entry, "--live", "--config")
	if err != nil && !strings.Contains(string(out), "no ") && !strings.Contains(err.Error(), "matching") {
		return fmt.Errorf("net-update delete host %s on %s: %w", host.MAC, network, err)
	}
	return nil
}

// InspectNetwork reads the network's persistent definition (range + static
// reservations) and its live dynamic leases. A network that isn't defined
// yields NetworkInfo{Exists: false} with no error.
func (p *LibvirtNetworkProvisioner) InspectNetwork(ctx context.Context, name string) (interfaces.NetworkInfo, error) {
	xml, err := p.virsh(ctx, "net-dumpxml", name)
	if err != nil {
		// net-dumpxml fails only when the network isn't defined.
		return interfaces.NetworkInfo{Exists: false}, nil
	}
	info := interfaces.NetworkInfo{Exists: true}
	info.DHCPRangeStart, info.DHCPRangeEnd = parseDHCPRange(string(xml))
	info.Reservations = parseReservations(string(xml))
	// Leases are best-effort: a stopped network has no lease table, which is
	// not an error for our purposes.
	if out, lerr := p.virsh(ctx, "net-dhcp-leases", name); lerr == nil {
		info.Leases = parseLeases(string(out))
	}
	return info, nil
}

// ResetNetwork stops and undefines the network so a subsequent EnsureNetwork
// rebuilds it from the current definition (fixing an outdated DHCP range and
// flushing stale dynamic leases). Idempotent.
func (p *LibvirtNetworkProvisioner) ResetNetwork(ctx context.Context, name string) error {
	// net-destroy fails if already stopped; ignore that and undefine anyway.
	_, _ = p.virsh(ctx, "net-destroy", name)
	out, err := p.virsh(ctx, "net-undefine", name)
	if err != nil && !strings.Contains(string(out), "not found") && !strings.Contains(err.Error(), "Network not found") {
		return fmt.Errorf("net-undefine %s: %w", name, err)
	}
	return nil
}

var (
	reDHCPRange = regexp.MustCompile(`<range\s+start='([^']+)'\s+end='([^']+)'`)
	reHostLine  = regexp.MustCompile(`<host\b[^>]*/?>`)
	reAttrMAC   = regexp.MustCompile(`mac='([^']+)'`)
	reAttrIP    = regexp.MustCompile(`ip='([^']+)'`)
	reAttrName  = regexp.MustCompile(`name='([^']*)'`)
)

func parseDHCPRange(xml string) (start, end string) {
	if m := reDHCPRange.FindStringSubmatch(xml); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

func parseReservations(xml string) []interfaces.DHCPHost {
	var hosts []interfaces.DHCPHost
	for _, line := range reHostLine.FindAllString(xml, -1) {
		h := interfaces.DHCPHost{}
		if m := reAttrMAC.FindStringSubmatch(line); m != nil {
			h.MAC = m[1]
		}
		if m := reAttrIP.FindStringSubmatch(line); m != nil {
			h.IP = m[1]
		}
		if m := reAttrName.FindStringSubmatch(line); m != nil {
			h.Hostname = m[1]
		}
		if h.MAC != "" || h.IP != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// parseLeases parses the `virsh net-dhcp-leases` table. Best-effort: the format
// is " <date> <time> <mac> <proto> <ip>/<prefix> <hostname> <clientid>", and
// rows that don't fit are skipped.
func parseLeases(out string) []interfaces.DHCPLease {
	var leases []interfaces.DHCPLease
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// Need at least date, time, mac, proto, ip.
		if len(f) < 5 || !strings.Contains(f[2], ":") {
			continue
		}
		ip := f[4]
		if i := strings.IndexByte(ip, '/'); i >= 0 {
			ip = ip[:i]
		}
		lease := interfaces.DHCPLease{MAC: f[2], IP: ip}
		if len(f) >= 6 && f[5] != "-" {
			lease.Hostname = f[5]
		}
		leases = append(leases, lease)
	}
	return leases
}

// buildNetworkXML assembles the shared NAT network definition (no
// reservations — those are added via net-update). The <domain> element is
// included only when spec.Domain is set (omitted under magic DNS so dnsmasq
// forwards the wildcard-service queries upstream). The bridge interface name
// is omitted unless a short one is supplied, so libvirt auto-assigns virbrN
// (a bridge ifname is capped at 15 chars; the network name is not).
func buildNetworkXML(spec interfaces.NetworkSpec) string {
	var domain string
	if spec.Domain != "" {
		domain = fmt.Sprintf("\n  <domain name='%s' localOnly='yes'/>", spec.Domain)
	}
	bridge := "<bridge stp='on' delay='0'/>"
	if spec.Bridge != "" {
		bridge = fmt.Sprintf("<bridge name='%s' stp='on' delay='0'/>", spec.Bridge)
	}
	return fmt.Sprintf(`<network>
  <name>%s</name>
  <forward mode='nat'>
    <nat>
      <port start='1024' end='65535'/>
    </nat>
  </forward>
  %s%s
  <ip address='%s.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='%s.%d' end='%s.%d'/>
    </dhcp>
  </ip>
</network>`, spec.Name, bridge, domain, spec.Subnet,
		spec.Subnet, config.DHCPDynamicStart, spec.Subnet, config.DHCPDynamicEnd)
}

func writeTempFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}

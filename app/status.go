package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/TheEasyShift/easyshift/config"
)

// StatusCheck is one read-only diagnostic result.
type StatusCheck struct {
	Name   string
	OK     bool
	Detail string
	Hint   string // shown only when !OK
}

// StatusReport is the collected output of ClusterManager.Status.
type StatusReport struct {
	Cluster          *config.ClusterConfig
	FQDN             string
	Checks           []StatusCheck
	InstallerLogTail string
}

// Status runs read-only diagnostics: VM state, master reachability (bridge
// mode), DNS records, API port, plus the tail of the installer log.
func (cm *ClusterManager) Status(ctx context.Context, name string) (*StatusReport, error) {
	c := cm.find(name)
	if c == nil {
		return nil, fmt.Errorf("cluster %s not found", name)
	}
	rep := &StatusReport{Cluster: c, FQDN: c.FQDN()}

	rep.Checks = append(rep.Checks, cm.checkVMState(ctx, c))
	if c.NetworkMode == config.NetworkModeBridge {
		rep.Checks = append(rep.Checks, cm.checkARP(c))
		rep.Checks = append(rep.Checks, cm.checkDNS(ctx, c, rep.FQDN))
	}
	if ip := c.PrimaryMasterIP(); ip != "" {
		rep.Checks = append(rep.Checks, cm.checkAPIPort(ip))
	}
	rep.Checks = append(rep.Checks, cm.checkAPIDNSEnd2End(rep.FQDN))

	rep.InstallerLogTail = tailFile(installerLogPath(cm.cfg.ConfigDir, c.Name), 20)
	return rep, nil
}

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

func (cm *ClusterManager) checkARP(c *config.ClusterConfig) StatusCheck {
	ip, err := cm.deps.Host.ARPLookup(c.MasterMAC)
	if err != nil {
		return StatusCheck{Name: "ARP for master MAC", Detail: err.Error()}
	}
	if ip == "" {
		return StatusCheck{
			Name:   "ARP for master MAC",
			Detail: "no entry (host has not seen this MAC)",
			Hint:   "try `ping -c2 " + c.MasterIP + "` to populate ARP, then re-run status",
		}
	}
	if ip != c.MasterIP {
		return StatusCheck{
			Name:   "ARP for master MAC",
			Detail: fmt.Sprintf("MAC %s is at %s, expected %s", c.MasterMAC, ip, c.MasterIP),
			Hint:   "the router did not honor your DHCP reservation; fix the reservation so MAC " + c.MasterMAC + " leases " + c.MasterIP,
		}
	}
	return StatusCheck{Name: "ARP for master MAC", OK: true, Detail: fmt.Sprintf("%s -> %s", c.MasterMAC, ip)}
}

func (cm *ClusterManager) checkDNS(ctx context.Context, c *config.ClusterConfig, fqdn string) StatusCheck {
	var bad []string
	for _, name := range config.ClusterDNSNames(fqdn) {
		ips, err := cm.deps.DNS.Resolve(ctx, name)
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: lookup failed: %v", name, err))
		case len(ips) == 0:
			bad = append(bad, fmt.Sprintf("%s: no records", name))
		case !containsStr(ips, c.MasterIP):
			bad = append(bad, fmt.Sprintf("%s: %v (want %s)", name, ips, c.MasterIP))
		}
	}
	if len(bad) == 0 {
		return StatusCheck{Name: "DNS records", OK: true, Detail: "all 3 records resolve to " + c.MasterIP}
	}
	return StatusCheck{
		Name:   "DNS records",
		Detail: strings.Join(bad, "; "),
		Hint: "add A records for api.<fqdn>, api-int.<fqdn>, *.apps.<fqdn> pointing at " + c.MasterIP +
			" (and, optionally per OpenShift docs, a PTR for " + c.MasterIP + " -> master-0.<fqdn>)",
	}
}

func (cm *ClusterManager) checkAPIPort(masterIP string) StatusCheck {
	addr := masterIP + ":6443"
	if err := cm.deps.Host.DialTCP(addr, 3*time.Second); err != nil {
		return StatusCheck{
			Name:   "API port 6443 (by IP)",
			Detail: err.Error(),
			Hint:   "if the VM is up, the API may still be bootstrapping; check `sudo virsh console master-0-<cluster>` for kubelet/etcd progress",
		}
	}
	return StatusCheck{Name: "API port 6443 (by IP)", OK: true, Detail: "connected to " + addr}
}

func (cm *ClusterManager) checkAPIDNSEnd2End(fqdn string) StatusCheck {
	addr := "api." + fqdn + ":6443"
	if err := cm.deps.Host.DialTCP(addr, 3*time.Second); err != nil {
		return StatusCheck{
			Name:   "API via DNS",
			Detail: err.Error(),
			Hint:   "either DNS is wrong or the IP is unreachable; see the DNS-records and API-port checks above",
		}
	}
	return StatusCheck{Name: "API via DNS", OK: true, Detail: "connected to " + addr}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func installerLogPath(configDir, clusterName string) string {
	return configDir + "/clusters/" + clusterName + "/.openshift_install.log"
}

func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Print writes a human-readable report to w.
func (r *StatusReport) Print(w io.Writer) {
	fmt.Fprintf(w, "Cluster: %s (%s mode)\n", r.Cluster.Name, r.Cluster.NetworkMode)
	fmt.Fprintf(w, "FQDN:    %s\n", r.FQDN)
	if r.Cluster.MasterIP != "" {
		fmt.Fprintf(w, "Master:  %s  %s\n", r.Cluster.MasterIP, r.Cluster.MasterMAC)
	}
	fmt.Fprintf(w, "\nChecks:\n")
	for _, c := range r.Checks {
		marker := "[FAIL]"
		if c.OK {
			marker = "[ OK ]"
		}
		fmt.Fprintf(w, "  %s  %s: %s\n", marker, c.Name, c.Detail)
		if !c.OK && c.Hint != "" {
			fmt.Fprintf(w, "         hint: %s\n", c.Hint)
		}
	}
	if r.InstallerLogTail != "" {
		fmt.Fprintf(w, "\nLast %d lines of .openshift_install.log:\n%s\n",
			strings.Count(r.InstallerLogTail, "\n")+1, r.InstallerLogTail)
	}
}

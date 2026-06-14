// Package publishpxeassets is the macOS boot-media stage: it publishes the
// RHCOS live rootfs and the cluster ignition into the HTTP fileserver root, and
// records the kernel cmdline (ignition.config.url + rootfs url) on the cluster
// for create-master-vms to pass to vfkit's Linux bootloader. It replaces
// embed-ignition-iso on macOS (which uses coreos-installer + a libvirt storage
// pool, neither available on mac). The kernel + initramfs are passed to vfkit
// directly by local path (from the RHCOS cache), so only the rootfs and
// ignition need HTTP serving.
//
// macOS has no coreos-installer to embed a static-network keyfile into the
// live ISO, so to pin the master to its allocated IP (rather than a vmnet DHCP
// address) we inject a NetworkManager keyfile directly into the served
// ignition. Without it the node would DHCP an arbitrary address and the API
// would not be reachable at the magic-DNS name.
package publishpxeassets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

// Stage publishes PXE-style boot assets.
type Stage struct {
	files interfaces.FileServer
}

// New returns the publish-pxe-assets stage.
func New(files interfaces.FileServer) *Stage { return &Stage{files: files} }

func (*Stage) Name() string { return "publish-pxe-assets" }

// KernelCmdline builds the vfkit Linux-bootloader cmdline: serial console on
// hvc0, the RHCOS live rootfs and the per-cluster ignition both fetched from
// the fileserver, and the metal ignition platform with firstboot.
func KernelCmdline(baseURL, cluster string) string {
	return fmt.Sprintf(
		"console=hvc0 coreos.live.rootfs_url=%s/%s/rootfs.img ignition.firstboot ignition.platform.id=metal ignition.config.url=%s/%s/config.ign",
		baseURL, cluster, baseURL, cluster,
	)
}

// Apply copies the RHCOS rootfs and the SNO ignition (with a static-network
// keyfile injected to pin the master IP) into <fileserver-root>/<cluster>/, and
// records the install-phase cmdline on the cluster for create-master-vms.
func (s *Stage) Apply(_ context.Context, sc *interfaces.StageContext) error {
	cluster := sc.Cluster.Name
	dstDir := filepath.Join(s.files.RootDir(), cluster)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("publish-pxe-assets: mkdir %s: %w", dstDir, err)
	}
	if err := copyFile(sc.RHCOSRootfsPath(), filepath.Join(dstDir, "rootfs.img")); err != nil {
		return fmt.Errorf("publish rootfs: %w", err)
	}

	ign, err := os.ReadFile(filepath.Join(sc.ClusterDir(), "bootstrap-in-place-for-live-iso.ign"))
	if err != nil {
		return fmt.Errorf("read SNO ignition: %w", err)
	}
	merged, err := injectStaticNetwork(ign, sc.Cluster)
	if err != nil {
		return fmt.Errorf("inject static network: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "config.ign"), merged, 0o600); err != nil {
		return fmt.Errorf("publish ignition: %w", err)
	}

	sc.Cluster.InstallKernelCmdline = KernelCmdline(s.files.BaseURL(), cluster)
	return nil
}

// Rollback removes the published per-cluster assets.
func (s *Stage) Rollback(_ context.Context, sc *interfaces.StageContext) error {
	_ = os.RemoveAll(filepath.Join(s.files.RootDir(), sc.Cluster.Name))
	return nil
}

// injectStaticNetwork adds a NetworkManager keyfile (pinning the master to its
// allocated IP on the shared NAT subnet) into the ignition's storage.files,
// preserving every other field. NetworkManager applies the keyfile during
// ignition, so the control plane binds the allocated IP rather than a vmnet
// DHCP lease.
func injectStaticNetwork(ignition []byte, c *config.ClusterConfig) ([]byte, error) {
	keyfile := natKeyfile(c)
	entry := map[string]any{
		"path":      "/etc/NetworkManager/system-connections/" + connID(c) + ".nmconnection",
		"mode":      0o600,
		"overwrite": true,
		"contents":  map[string]any{"source": "data:;base64," + base64.StdEncoding.EncodeToString([]byte(keyfile))},
	}
	var m map[string]any
	if err := json.Unmarshal(ignition, &m); err != nil {
		return nil, fmt.Errorf("parse ignition json: %w", err)
	}
	storage, _ := m["storage"].(map[string]any)
	if storage == nil {
		storage = map[string]any{}
		m["storage"] = storage
	}
	files, _ := storage["files"].([]any)
	storage["files"] = append(files, entry)
	return json.Marshal(m)
}

func connID(c *config.ClusterConfig) string { return "master-0-" + c.Name }

// natKeyfile renders the NetworkManager keyfile pinning the master to its
// allocated IP on the shared NAT subnet (gateway/DNS = the vmnet gateway .1).
func natKeyfile(c *config.ClusterConfig) string {
	gw := config.BaseNetworkRange + ".1"
	return fmt.Sprintf(`[connection]
id=%s
type=ethernet
autoconnect=true
autoconnect-priority=999

[ethernet]
mac-address=%s

[ipv4]
method=manual
address1=%s/24,%s
dns=%s;
may-fail=false

[ipv6]
method=disabled
`, connID(c), strings.ToUpper(c.PrimaryMasterMAC()), c.PrimaryMasterIP(), gw, gw)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

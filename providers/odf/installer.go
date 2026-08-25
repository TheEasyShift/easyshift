package odf

// installer.go implements interfaces.ODFInstaller: driving the manifests
// rendered in render.go against a running cluster via `oc`. Every mutation
// goes through the CommandRunner (--simulate traces it, tests assert exact
// invocations); nothing here talks to Kubernetes directly.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TheEasyShift/easyshift/interfaces"
)

// Poll timeouts for each phase, pinned by the 2026-08-25 hardware spike.
// Package-level vars (not consts) so tests can shrink them instead of
// waiting out the real durations.
var (
	waitCSV            = 15 * time.Minute // lvms + ocs-operator CSVs, StorageCluster CRD
	waitLVM            = 5 * time.Minute  // LVMCluster status.state == Ready
	waitSettle         = 5 * time.Minute  // ocs-operator deployment restarts into SINGLE_NODE
	waitStorageCluster = 30 * time.Minute // StorageCluster status.phase == Ready
	waitStorageClasses = 10 * time.Minute // ocs-client-operator creates the ceph StorageClasses
	pollInterval       = 10 * time.Second
)

// Installer implements interfaces.ODFInstaller by shelling out to `oc`
// through a CommandRunner.
type Installer struct {
	cmd interfaces.CommandRunner
}

// New returns an Installer that drives oc through cmd.
func New(cmd interfaces.CommandRunner) *Installer {
	return &Installer{cmd: cmd}
}

// InstallOperators applies the LVMS + ODF Subscriptions and waits for both
// operators' CSVs to succeed and for ODF's CRDs to land. The ocs-operator
// CSV arrives via odf-operator's dependent subscriptions, which is why the
// wait is on ocs-operator's CSV rather than just odf-operator's.
func (i *Installer) InstallOperators(ctx context.Context, spec interfaces.ODFSpec) error {
	if err := i.apply(ctx, spec, "01-operators.yaml", RenderOperators(spec.Channel)); err != nil {
		return err
	}
	return i.pollUntil(ctx, waitCSV, "lvms-operator + ocs-operator CSVs and the StorageCluster CRD", func() (bool, error) {
		ok, err := i.csvSucceeded(ctx, spec, lvmsNamespace, "lvms-operator")
		if err != nil || !ok {
			return false, err
		}
		ok, err = i.csvSucceeded(ctx, spec, odfNamespace, "ocs-operator")
		if err != nil || !ok {
			return false, err
		}
		if _, err := i.oc(ctx, spec, "get", "crd", "storageclusters.ocs.openshift.io"); err != nil {
			return false, err
		}
		return true, nil
	})
}

// SetupLVM applies the LVMCluster (waiting for it to reach Ready) and then
// easyshift's own Immediate-binding StorageClass on top of it.
func (i *Installer) SetupLVM(ctx context.Context, spec interfaces.ODFSpec) error {
	if err := i.apply(ctx, spec, "02-lvmcluster.yaml", RenderLVMCluster(spec.DevicePath)); err != nil {
		return err
	}
	if err := i.pollUntil(ctx, waitLVM, "LVMCluster easyshift-odf Ready", func() (bool, error) {
		state, err := i.ocJSONPath(ctx, spec, "{.status.state}", "get", "lvmcluster", "easyshift-odf", "-n", lvmsNamespace)
		if err != nil {
			return false, err
		}
		return state == "Ready", nil
	}); err != nil {
		return err
	}
	return i.apply(ctx, spec, "03-storageclass-immediate.yaml", RenderImmediateStorageClass())
}

// EnableSingleNode flips the ocs-operator Subscription into single-node
// mode, waits for the resulting restart to settle, labels the (only) node
// for openshift-storage, and applies the CephCSI Driver CRs plus the
// monitoring trim — both must land before the StorageCluster is created.
func (i *Installer) EnableSingleNode(ctx context.Context, spec interfaces.ODFSpec) error {
	// The Subscription odf-operator creates for ocs-operator has a
	// catalog-decorated metadata.name; it must be found by spec.name, not
	// guessed from metadata.name.
	subName, err := i.ocJSONPath(ctx, spec,
		`{range .items[?(@.spec.name=="ocs-operator")]}{.metadata.name}{end}`,
		"get", "sub", "-n", odfNamespace)
	if err != nil {
		return fmt.Errorf("find ocs-operator subscription: %w", err)
	}
	if subName == "" {
		return fmt.Errorf("no Subscription with spec.name=ocs-operator found in %s", odfNamespace)
	}

	if _, err := i.oc(ctx, spec, "patch", "sub", "-n", odfNamespace, subName,
		"--type", "merge", "-p", SingleNodePatch()); err != nil {
		return fmt.Errorf("patch subscription %s: %w", subName, err)
	}

	if err := i.pollUntil(ctx, waitSettle, "ocs-operator deployment SINGLE_NODE + Available", func() (bool, error) {
		env, err := i.ocJSONPath(ctx, spec,
			`{range .spec.template.spec.containers[*]}{range .env[?(@.name=="SINGLE_NODE")]}{.value}{end}{end}`,
			"get", "deploy", "ocs-operator", "-n", odfNamespace)
		if err != nil {
			return false, err
		}
		if env != "true" {
			return false, nil
		}
		avail, err := i.ocJSONPath(ctx, spec,
			`{range .status.conditions[?(@.type=="Available")]}{.status}{end}`,
			"get", "deploy", "ocs-operator", "-n", odfNamespace)
		if err != nil {
			return false, err
		}
		return avail == "True", nil
	}); err != nil {
		return err
	}

	if _, err := i.oc(ctx, spec, "label", "nodes", "--all",
		"cluster.ocs.openshift.io/openshift-storage=", "--overwrite"); err != nil {
		return fmt.Errorf("label node for openshift-storage: %w", err)
	}

	if err := i.apply(ctx, spec, "04-driver-rbd.yaml", RenderDriver("openshift-storage.rbd.csi.ceph.com")); err != nil {
		return err
	}
	if err := i.apply(ctx, spec, "05-driver-cephfs.yaml", RenderDriver("openshift-storage.cephfs.csi.ceph.com")); err != nil {
		return err
	}
	return i.apply(ctx, spec, "06-monitoring-trim.yaml", RenderMonitoringTrim())
}

// CreateStorageCluster applies the StorageCluster CR and waits for it to
// reach Ready, then waits for ocs-client-operator to have asynchronously
// created both ceph StorageClasses — Ready alone is not completion.
func (i *Installer) CreateStorageCluster(ctx context.Context, spec interfaces.ODFSpec) error {
	if err := i.apply(ctx, spec, "07-storagecluster.yaml", RenderStorageCluster(spec.DataPVCSizeGi)); err != nil {
		return err
	}
	if err := i.pollUntil(ctx, waitStorageCluster, "StorageCluster ocs-storagecluster Ready", func() (bool, error) {
		phase, err := i.ocJSONPath(ctx, spec, "{.status.phase}", "get", "storagecluster", "ocs-storagecluster", "-n", odfNamespace)
		if err != nil {
			return false, err
		}
		return phase == "Ready", nil
	}); err != nil {
		return err
	}
	return i.pollUntil(ctx, waitStorageClasses, "ceph StorageClasses "+RBDStorageClass+" and "+CephFSStorageClass, func() (bool, error) {
		if _, err := i.oc(ctx, spec, "get", "sc", RBDStorageClass); err != nil {
			return false, err
		}
		if _, err := i.oc(ctx, spec, "get", "sc", CephFSStorageClass); err != nil {
			return false, err
		}
		return true, nil
	})
}

// apply writes content to <spec.WorkDir>/odf/<filename> — CommandRunner has
// no stdin, so files are both the transport and a debugging artifact — then
// applies it.
func (i *Installer) apply(ctx context.Context, spec interfaces.ODFSpec, filename, content string) error {
	dir := filepath.Join(spec.WorkDir, "odf")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := i.oc(ctx, spec, "apply", "-f", path); err != nil {
		return fmt.Errorf("apply %s: %w", filename, err)
	}
	return nil
}

// oc runs `spec.OCBinaryPath --kubeconfig spec.KubeconfigPath <args...>`.
func (i *Installer) oc(ctx context.Context, spec interfaces.ODFSpec, args ...string) ([]byte, error) {
	full := append([]string{"--kubeconfig", spec.KubeconfigPath}, args...)
	return i.cmd.Run(ctx, spec.OCBinaryPath, full...)
}

// ocJSONPath runs an oc get with -o jsonpath=<jsonpath> appended to args and
// returns the trimmed output.
func (i *Installer) ocJSONPath(ctx context.Context, spec interfaces.ODFSpec, jsonpath string, args ...string) (string, error) {
	full := append(append([]string(nil), args...), "-o", "jsonpath="+jsonpath)
	out, err := i.oc(ctx, spec, full...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// csvSucceeded lists the CSVs in namespace and reports whether any whose
// name has namePrefix (CSV names are version-decorated, e.g.
// "ocs-operator.v4.22.2") has reached status.phase Succeeded.
func (i *Installer) csvSucceeded(ctx context.Context, spec interfaces.ODFSpec, namespace, namePrefix string) (bool, error) {
	out, err := i.oc(ctx, spec, "get", "csv", "-n", namespace,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.phase}{"\n"}{end}`)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, phase, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, namePrefix) && phase == "Succeeded" {
			return true, nil
		}
	}
	return false, nil
}

// pollUntil calls probe every pollInterval until it reports true, or until
// timeout elapses, in which case it returns an error naming desc. A probe
// error is treated as a transient API flap and retried rather than failing
// the poll immediately.
func (i *Installer) pollUntil(ctx context.Context, timeout time.Duration, desc string, probe func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := probe()
		if err == nil && ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, desc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

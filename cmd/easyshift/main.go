package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"github.com/TheEasyShift/easyshift/app"
	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
	"github.com/TheEasyShift/easyshift/providers/fakes"
	"github.com/spf13/cobra"
)

// annotationNeedsFileServer marks subcommands that need the HTTP file server
// running. Only commands that may have VMs fetch ignition or images set this;
// read-only commands like list/status skip it so they don't race the create
// command for port 9393.
const annotationNeedsFileServer = "easyshift/needs-file-server"

func main() {
	// easyshift does not require running as root. Both NAT and bridge mode
	// need only libvirt-group membership; the libvirt-reachability preflight
	// on stages that touch qemu:///system will surface the right error if the
	// user is missing permissions. NAT mode resolves names via magic DNS
	// (sslip.io/nip.io), so nothing under /etc/ is ever written.

	var (
		debug    bool
		simulate bool
	)

	configDir := config.DefaultConfigDir()
	cfg, err := config.LoadConfig(configDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	var mgr *app.ClusterManager
	var deps interfaces.Deps
	var simBundle *fakes.Bundle // set only when --simulate

	rootCmd := &cobra.Command{
		Use:   "easyshift",
		Short: "easyshift - Easy OpenShift Cluster Management",
		Long: `easyshift is a tool for creating and managing OpenShift clusters.
It provides a simple interface for cluster lifecycle management.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if simulate {
				// Swap in a throwaway config dir so we never touch real state
				// (config.json, ~/.config/easyshift/clusters/*, ACME accounts, etc.).
				simCfg, simDeps, bundle, err := newSimulationEnv(cfg)
				if err != nil {
					return fmt.Errorf("simulate: %w", err)
				}
				cfg, deps, simBundle = simCfg, simDeps, bundle
				if err := config.InitLogging(cfg.LogFile, debug); err != nil {
					return err
				}
				mgr = app.NewClusterManager(cfg, deps)
				return nil
			}
			if err := config.InitLogging(cfg.LogFile, debug); err != nil {
				return err
			}
			host, err := primaryHostIP()
			if err != nil {
				return fmt.Errorf("detect host IP: %w", err)
			}
			deps, err = app.NewProductionDeps(cfg, host)
			if err != nil {
				return err
			}
			if cmd.Annotations[annotationNeedsFileServer] == "true" {
				if err := deps.Files.Start(context.Background()); err != nil {
					return fmt.Errorf("start file server: %w", err)
				}
			}
			mgr = app.NewClusterManager(cfg, deps)
			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			if simulate && simBundle != nil {
				fmt.Fprintln(os.Stdout)
				simBundle.WriteTrace(os.Stdout)
				return nil
			}
			if cmd.Annotations[annotationNeedsFileServer] == "true" && deps.Files != nil {
				_ = deps.Files.Stop(context.Background())
			}
			return nil
		},
	}

	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug logging")
	rootCmd.PersistentFlags().BoolVarP(&simulate, "simulate", "S", false,
		"Run against in-memory fakes instead of real libvirt/Cloudflare/Let's Encrypt. "+
			"Uses a throwaway config dir; prints a trace of every operation the real run would have performed.")

	rootCmd.AddCommand(
		newCreateCommand(&mgr, &simBundle, &cfg, &deps),
		newStartCommand(&mgr),
		newStopCommand(&mgr),
		newDeleteCommand(&mgr),
		newListCommand(&mgr),
		newStatusCommand(&mgr),
		newNATNetworkCommand(&mgr),
		newPullSecretCommand(&cfg, &deps),
		newDNSCommand(cfg),
		newTrustCommand(&cfg, &deps),
	)

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

func newCreateCommand(mgr **app.ClusterManager, simBundle **fakes.Bundle, cfgp **config.Config, depsp *interfaces.Deps) *cobra.Command {
	var (
		name        string
		baseDomain  string
		ocpVersion  string
		masterCount int
		workerCount int
		masterRAM   int
		workerRAM   int
		masterDisk  int
		masterCPUs  int
		networkMode string
		bridge      string
		masterMAC   string
		masterIP    string
		machineCIDR string
		gateway     string
		dns         string
		storagePool string
		dnsProvider string
		dnsZone     string
		tlsEmail    string
		tlsStaging  bool
		magicDNS    string
		bakeImages  bool
		odf         bool
		odfDisk     int
	)

	cmd := &cobra.Command{
		Use:         "create",
		Short:       "Create a new OpenShift cluster",
		Annotations: map[string]string{annotationNeedsFileServer: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c := &config.ClusterConfig{
				Name:         name,
				Domain:       baseDomain,
				OCPVersion:   ocpVersion,
				MasterCount:  masterCount,
				WorkerCount:  workerCount,
				MasterRAM:    masterRAM,
				WorkerRAM:    workerRAM,
				MasterDiskGB: masterDisk,
				MasterCPUs:   masterCPUs,
				NetworkMode:  networkMode,
				Bridge:       bridge,
				MasterMAC:    masterMAC,
				MasterIP:     masterIP,
				MachineCIDR:  machineCIDR,
				Gateway:      gateway,
				DNS:          dns,
				StoragePool:  storagePool,
				DNSProvider:  dnsProvider,
				DNSZone:      dnsZone,
				TLSEmail:     tlsEmail,
				TLSStaging:   tlsStaging,
				MagicDNS:     magicDNS,
				BakeImages:   bakeImages,
				ODF:          odf,
				ODFDiskGB:    odfDisk,
			}
			// In a bridge-mode simulation there is no real node, so pretend it
			// came up on its reserved IP — otherwise the verify-master-ip stage
			// would poll the fake host until it times out.
			if simBundle != nil && *simBundle != nil && c.NetworkMode == config.NetworkModeBridge {
				(*simBundle).Host.ARPTable = map[string]string{c.MasterMAC: c.MasterIP}
			}
			if err := ensurePullSecret(context.Background(), *cfgp, depsp.PullSecret, os.Stdin, os.Stdout, stdinIsTTY()); err != nil {
				return err
			}
			if err := (*mgr).Create(context.Background(), c); err != nil {
				return err
			}
			// Create may have resumed an existing cluster (a different
			// *ClusterConfig than ours) — re-find it for accurate fields.
			for _, cl := range (*mgr).List() {
				if cl.Name == name {
					printCreateSummary(os.Stdout, *cfgp, cl)
					break
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&name, "name", "n", "", "Cluster name")
	cmd.Flags().StringVarP(&baseDomain, "base-domain", "D", "",
		"OpenShift baseDomain; the cluster's API and ingress live under <name>.<base-domain>. "+
			"Defaults to 'local' (bridge) or a derived <ip>.sslip.io (NAT, via --magic-dns). "+
			"Set it to use your own domain (disables --magic-dns auto).")
	cmd.Flags().StringVarP(&ocpVersion, "version", "v", config.DefaultOCPVersion, "OpenShift version")
	cmd.Flags().IntVarP(&masterCount, "masters", "m", 1, "Number of master nodes (must be 1)")
	cmd.Flags().IntVarP(&workerCount, "workers", "w", 0, "Number of worker nodes (Phase 1: must be 0; add later via addnode)")
	cmd.Flags().IntVar(&masterRAM, "master-ram", 32768, "Master node RAM in MB")
	cmd.Flags().IntVar(&masterDisk, "master-disk", 120,
		"Master node disk size in GB (sparse; OpenShift recommends 120, smaller fits dev hosts with tight disks)")
	cmd.Flags().IntVar(&workerRAM, "worker-ram", 16384, "Worker node RAM in MB")
	cmd.Flags().StringVar(&networkMode, "network-mode", config.NetworkModeNAT,
		"Network mode: 'nat' (libvirt NAT + HAProxy on host) or 'bridge' (attach to a host Linux bridge on the LAN)")
	cmd.Flags().StringVar(&bridge, "bridge", "",
		"Name of an existing host Linux bridge (e.g. br0); required when --network-mode=bridge")
	cmd.Flags().StringVar(&masterMAC, "master-mac", "",
		"MAC address you reserved at the router for the master VM; required in bridge mode")
	cmd.Flags().StringVar(&masterIP, "master-ip", "",
		"IP the router will hand to --master-mac; required in bridge mode")
	cmd.Flags().StringVar(&machineCIDR, "machine-cidr", "",
		"Override the LAN CIDR for networking.machineNetwork in install-config; defaults to /24 of --master-ip")
	cmd.Flags().StringVar(&gateway, "gateway", "",
		"Bridge mode: default gateway for the master's static network config; defaults to the .1 of --machine-cidr")
	cmd.Flags().StringVar(&dns, "dns", "",
		"Bridge mode: comma-separated DNS servers for the master's static network config; defaults to --gateway")
	cmd.Flags().StringVar(&storagePool, "storage-pool", config.DefaultStoragePool,
		"libvirt storage pool for the master disk and boot ISO (run `virsh pool-list --all` to see yours)")
	cmd.Flags().StringVar(&dnsProvider, "dns-provider", "",
		"Public DNS provider to use for api/api-int/*.apps records (currently: 'cloudflare'). "+
			"Empty disables automation: you create the records yourself. Token must be set first via `easyshift dns set <provider>`.")
	cmd.Flags().StringVar(&dnsZone, "dns-zone", "",
		"Parent DNS zone (defaults to --base-domain). Override when the zone is a parent of --base-domain.")
	cmd.Flags().StringVar(&tlsEmail, "tls-email", "",
		"ACME account email; non-empty enables Let's Encrypt cert issuance for api.<fqdn> and *.apps.<fqdn> "+
			"via DNS-01. Requires --dns-provider (the same token is reused for the challenge).")
	cmd.Flags().BoolVar(&tlsStaging, "tls-staging", false,
		"Use Let's Encrypt's staging endpoint (untrusted certs, but no production rate limits). "+
			"Recommended while iterating; flip to false for the final run.")
	cmd.Flags().StringVar(&magicDNS, "magic-dns", config.MagicDNSAuto,
		"Wildcard DNS service so cluster names resolve to the master IP with no records to manage: "+
			"'auto' (NAT -> sslip.io, bridge -> off), 'sslip.io', 'nip.io', or 'off'. "+
			"Mutually exclusive with --dns-provider.")
	cmd.Flags().BoolVar(&bakeImages, "bake-images", false,
		"Pre-pull the entire OCP release payload into a read-only disk attached to the master so "+
			"the install never reaches quay.io for platform images. Built once per version (multi-arch: "+
			"amd64 + aarch64), shared across clusters. Needs skopeo + virt-make-fs on PATH.")
	cmd.Flags().IntVar(&masterCPUs, "master-cpus", 4, "Master node vCPUs (raised to 8 automatically with --odf)")
	cmd.Flags().BoolVar(&odf, "odf", false,
		"Install OpenShift Data Foundation after the cluster is up: a dedicated data disk, "+
			"LVMS thin pool, and a single-node Ceph StorageCluster (RBD + CephFS StorageClasses). "+
			"Raises the master to 8 vCPUs / 19456 MiB RAM. Needs the default OperatorHub catalogs (online).")
	cmd.Flags().IntVar(&odfDisk, "odf-disk", 0,
		"ODF backing data disk size in GB (sparse; default 100; usable Ceph capacity is about a third). Requires --odf.")

	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newStartCommand(mgr **app.ClusterManager) *cobra.Command {
	return &cobra.Command{
		Use:   "start [cluster-name]",
		Short: "Start a cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := (*mgr).Start(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Printf("cluster %s started: API up, node Ready, CSRs approved\n", args[0])
			return nil
		},
	}
}

func newStopCommand(mgr **app.ClusterManager) *cobra.Command {
	return &cobra.Command{
		Use:   "stop [cluster-name]",
		Short: "Stop a cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*mgr).Stop(context.Background(), args[0])
		},
	}
}

func newDeleteCommand(mgr **app.ClusterManager) *cobra.Command {
	return &cobra.Command{
		Use:   "delete [cluster-name]",
		Short: "Delete a cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return (*mgr).Delete(context.Background(), args[0])
		},
	}
}

func newListCommand(mgr **app.ClusterManager) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all clusters",
		RunE: func(cmd *cobra.Command, args []string) error {
			clusters := (*mgr).List()
			if len(clusters) == 0 {
				fmt.Println("No clusters found")
				return nil
			}
			fmt.Println("Clusters:")
			for _, c := range clusters {
				fmt.Printf("- %s.%s  state=%s  version=%s  nodes=%dm/%dw\n",
					c.Name, c.Domain, c.State, c.OCPVersion, c.MasterCount, c.WorkerCount)
			}
			return nil
		},
	}
}

func newStatusCommand(mgr **app.ClusterManager) *cobra.Command {
	return &cobra.Command{
		Use:   "status <cluster-name>",
		Short: "Show diagnostic info for a cluster (VM, DNS, API reachability)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rep, err := (*mgr).Status(context.Background(), args[0])
			if err != nil {
				return err
			}
			rep.Print(os.Stdout)
			return nil
		},
	}
}

func newNATNetworkCommand(mgr **app.ClusterManager) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nat-network",
		Short: "Inspect and maintain the shared libvirt NAT network",
	}

	var (
		dryRun bool
		force  bool
	)
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Reconcile the shared NAT network with config: drop orphaned reservations/leaked allocations, and recreate it if its DHCP range is outdated",
		Long: "Compares the shared libvirt NAT network against the clusters easyshift knows about and\n" +
			"cleans up drift: DHCP reservations and IP/MAC allocations that belong to no cluster, plus a\n" +
			"network whose DHCP range predates the current layout (recreated so the corrected range applies,\n" +
			"which also flushes stale dynamic leases). Use --dry-run to see what would change.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			plan, err := (*mgr).PlanNATReset(context.Background(), force)
			if err != nil {
				return err
			}
			if dryRun {
				plan.Print(os.Stdout, false)
				return nil
			}
			if !plan.HasWork() {
				plan.Print(os.Stdout, false)
				return nil
			}
			if err := (*mgr).ApplyNATReset(context.Background(), plan); err != nil {
				return err
			}
			plan.Print(os.Stdout, true)
			return nil
		},
	}
	reset.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would change without modifying anything")
	reset.Flags().BoolVar(&force, "force", false, "Allow recreating the network even when NAT clusters are running (they briefly lose connectivity)")
	cmd.AddCommand(reset)
	return cmd
}

func newPullSecretCommand(cfgp **config.Config, depsp *interfaces.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull-secret",
		Short: "Manage the OpenShift pull secret used for cluster installs",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "set <file>",
		Short: "Store the pull secret from the given file (or '-' for stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return fmt.Errorf("read pull secret source: %w", err)
			}
			if err := config.WritePullSecret((*cfgp).ConfigDir, data); err != nil {
				return err
			}
			fmt.Printf("pull secret stored at %s\n", config.PullSecretPath((*cfgp).ConfigDir))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the path of the persisted pull secret",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := config.EnsurePullSecret((*cfgp).ConfigDir); err != nil {
				return err
			}
			fmt.Println(config.PullSecretPath((*cfgp).ConfigDir))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Fetch the pull secret from your Red Hat account (device-code login)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fetchAndStorePullSecret(context.Background(), *cfgp, depsp.PullSecret, os.Stdout)
		},
	})
	return cmd
}

func newDNSCommand(cfg *config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Manage credentials for the public DNS provider used to create cluster records",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "set <provider> <file>",
		Short: "Store an API token for the given provider (use '-' for stdin). Currently supported: cloudflare",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			provider, src := args[0], args[1]
			var data []byte
			var err error
			if src == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(src)
			}
			if err != nil {
				return fmt.Errorf("read token source: %w", err)
			}
			if err := config.WriteDNSToken(cfg.ConfigDir, provider, data); err != nil {
				return err
			}
			fmt.Printf("%s token stored at %s\n", provider, config.DNSTokenPath(cfg.ConfigDir, provider))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show <provider>",
		Short: "Print the path of the persisted token for the given provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := config.EnsureDNSToken(cfg.ConfigDir, args[0]); err != nil {
				return err
			}
			fmt.Println(config.DNSTokenPath(cfg.ConfigDir, args[0]))
			return nil
		},
	})
	return cmd
}

func newTrustCommand(cfgp **config.Config, depsp *interfaces.Deps) *cobra.Command {
	var uninstall bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Install the easyshift local CA into the host trust stores (uses sudo)",
		Long: "Installs the easyshift local CA — which signs the api/apps certificates of every\n" +
			"cluster created without --tls-email — into the system trust store (via sudo) and,\n" +
			"when certutil is available, the NSS databases used by Firefox and Chrome.\n" +
			"One-time per host; reversible with --uninstall.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTrust(context.Background(), *cfgp, depsp.TrustStore, uninstall, os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Remove the easyshift local CA from the host trust stores")
	return cmd
}

// newSimulationEnv returns a Config rooted at a throwaway temp dir and a Deps
// wired with fake implementations. The real cfg is read only for defaults
// (WebPort) — config.json on disk and persistent state are not touched.
// A fake pull secret and DNS token are pre-planted so the create pipeline
// makes it past the preflight checks that would otherwise require real
// credentials.
func newSimulationEnv(real *config.Config) (*config.Config, interfaces.Deps, *fakes.Bundle, error) {
	tmp, err := os.MkdirTemp("", "easyshift-sim-*")
	if err != nil {
		return nil, interfaces.Deps{}, nil, err
	}
	cfg := config.NewDefaultConfig(tmp)
	cfg.WebPort = real.WebPort
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return nil, interfaces.Deps{}, nil, err
	}
	// Pull-secret preflight rejects an absent file; plant a syntactically
	// valid stand-in so the pipeline progresses.
	if err := config.WritePullSecret(cfg.ConfigDir, []byte(`{"auths":{"sim":{"auth":"c2ltdWxhdGVk"}}}`)); err != nil {
		return nil, interfaces.Deps{}, nil, err
	}
	// Token for the cloudflare DNS provider — same reason.
	if err := config.WriteDNSToken(cfg.ConfigDir, config.DNSProviderCloudflare, []byte("simulated-token")); err != nil {
		return nil, interfaces.Deps{}, nil, err
	}
	deps, bundle := fakes.All()
	return cfg, deps, bundle, nil
}

// primaryHostIP returns the first non-loopback IPv4 address on the host.
func primaryHostIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}

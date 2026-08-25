# Usage

This is the command reference and the cluster lifecycle. For when to choose NAT
vs bridge, see **[networking.md](networking.md)**; for DNS/TLS automation, see
**[dns-and-tls.md](dns-and-tls.md)**.

## Global flags

| Flag | Meaning |
| --- | --- |
| `-d`, `--debug` | Verbose logging to `~/.config/easyshift/easyshift.log`. |
| `-S`, `--simulate` | Run the whole pipeline against in-memory fakes in a throwaway config dir, printing a trace of every operation a real run would perform. Touches no real libvirt/DNS/state. Great for a dry run. |

## Lifecycle at a glance

```
create ──> (running) ──> stop ──> (stopped) ──> start ──> (running)
   │                                                          │
   └──────────────────────── delete ◄────────────────────────┘
```

`create` is **idempotent and resumable**: re-running it for a non-running
cluster picks up at the first unfinished install stage. `delete` rolls back each
applied stage in reverse and removes the cluster directory.

## Commands

### `create` — provision a cluster

```sh
easyshift create --name demo                      # zero-config NAT + magic DNS
```

Only `--name` is required. Key flags:

| Flag | Default | Notes |
| --- | --- | --- |
| `-n`, `--name` | — | **Required.** Cluster name. |
| `-v`, `--version` | `stable` | OpenShift version, or a channel alias resolved against the mirror. Pass e.g. `4.21.0` to pin. |
| `--network-mode` | `nat` | `nat` or `bridge`. See [networking.md](networking.md). |
| `--magic-dns` | `auto` | `auto` / `sslip.io` / `nip.io` / `off`. Wildcard DNS so names resolve to the master IP with no records. Mutually exclusive with `--dns-provider` and `--base-domain`. |
| `-D`, `--base-domain` | — | Your own base domain (turns magic DNS off). Cluster lives at `<name>.<base-domain>`. |
| `--master-ram` | 32768 | Master RAM (MB). |
| `--storage-pool` | `default` | libvirt pool for the disk and ISO (`virsh pool-list --all`). |
| `-m`, `--masters` | 1 | Must be 1 (SNO). |
| `-w`, `--workers` | 0 | Must be 0 in the current phase. |

**Bridge-mode-only flags** (see [networking.md](networking.md)):

| Flag | Notes |
| --- | --- |
| `--bridge` | Name of an existing host Linux bridge (e.g. `br0`). Required for bridge mode. |
| `--master-mac` | MAC you reserved at your router for the master VM. Required. |
| `--master-ip` | IP the router will hand to that MAC. Required. |
| `--machine-cidr` | Override `machineNetwork`; defaults to the `/24` of `--master-ip`. |

**DNS / TLS flags** (see [dns-and-tls.md](dns-and-tls.md)):

| Flag | Notes |
| --- | --- |
| `--dns-provider` | `cloudflare` to auto-create `api`/`api-int`/`*.apps` records. Token must be set first. |
| `--dns-zone` | Parent zone, if different from `--base-domain`. |
| `--tls-email` | ACME account email; enables Let's Encrypt certs via DNS-01 (requires `--dns-provider`). |
| `--tls-staging` | Use Let's Encrypt staging (untrusted certs, no rate limits) while iterating. |

**Storage (`--odf`)**: installs [OpenShift Data Foundation](https://www.redhat.com/en/technologies/cloud-computing/openshift-data-foundation)
(Ceph via Rook) onto the single master node. You get the full ODF feature
set: working RBD and CephFS `StorageClasses`, S3-compatible object storage
(NooBaa Multicloud Gateway + Ceph RGW), and ODF's own monitoring — all backed
by a dedicated VM data disk and an in-node LVMS thin pool. A real Ceph
cluster reporting `HEALTH_OK`, not a shortcut: easyshift trims resource
*requests* to fit one node but never disables features. What you lose versus
production is the infrastructure (one node, one disk), not the features.

```sh
easyshift create --name demo --odf                  # default 100 GB data disk
easyshift create --name demo --odf --odf-disk 200    # bigger data disk
```

| Flag | Default | Notes |
| --- | --- | --- |
| `--odf` | off | Installs the storage stack after the cluster comes up. Requires the default OperatorHub catalogs (online) — OLM pulls `lvms-operator` and `odf-operator` from `redhat-operators`, so `--odf` won't work in an offline/disconnected setup. |
| `--odf-disk` | 100 | Size in GB of the sparse backing data disk for Ceph. Requires `--odf`. |
| `--master-cpus` | 4 | Master vCPUs; `--odf` raises this to 8 automatically if left lower. |

**Capacity math**: usable Ceph capacity is roughly `--odf-disk / 3` — three
OSDs share the one data disk, so PV-visible space works out to about a third
of what you asked for, not the whole disk.

**Resource floors**: `--odf` needs real headroom. It automatically raises
the master to **8 vCPUs and 24576 MiB RAM** if your `--master-ram` /
`--master-cpus` were lower (a log line tells you when it does). That floor
is hardware-validated, not a guess, and it leaves essentially no slack —
budget a **24 GB host as the practical minimum**, with zero headroom to
spare once the VM, host OS, and everything else on the box are accounted
for.

**No hardware redundancy**: the StorageCluster is configured with three
OSDs and `replica: 1` sharing the same single physical disk — Ceph-level
replica semantics with zero actual hardware redundancy. Losing that one disk
loses everything. This is a dev/test convenience, not a durability story.

See [docs/dev/odf.md](../dev/odf.md) for the stage internals and the exact
ODF 4.22 recipe deltas this is built on.

### `list` — show all clusters

```sh
easyshift list
# - demo.192.168.126.5.sslip.io  state=running  version=4.21.0  nodes=1m/0w
```

### `status <name>` — diagnose a cluster

Runs read-only checks and prints a report: VM state; (bridge mode) ARP for the
master MAC and that `api`/`api-int`/`*.apps` DNS resolves to the master; the API
port `6443` by IP; the API reachable via DNS; plus the tail of
`.openshift_install.log`. Each failing check includes a hint.

```sh
easyshift status demo
```

### `start` / `stop <name>`

```sh
easyshift stop demo     # graceful shutdown of all nodes
easyshift start demo    # boot them back up
```

`start` blocks until the API responds, pending CSRs are approved, and the node
is Ready (see [access.md](access.md)).

### `delete <name>`

Stops the cluster if running, rolls back every applied install stage (VMs,
libvirt artifacts, DNS records, IP/MAC reservations), and removes the cluster
directory.

```sh
easyshift delete demo
```

### `trust` — install the easyshift CA into host trust stores

Installs the host-local easyshift CA certificate into your system and browser
trust stores so that `curl`, `oc`, and your browser accept local-CA cluster
certificates without `--insecure-skip-tls-verify`. Requires `sudo` (or your
distro equivalent) to write to system certificate directories. Run once; all
current and future local-CA clusters are covered immediately. See
[access.md](access.md) for the full trust workflow.

```sh
easyshift trust              # install into system + browser trust stores (prompts for sudo)
easyshift trust --uninstall  # remove the CA from all trust stores
```

### `nat-network reset` — clean up the shared NAT network

Reconciles the shared libvirt NAT network (`easyshift-nat`) with the clusters
easyshift knows about, and repairs drift accumulated by crashed/aborted installs:

- removes DHCP reservations and `config.json` IP/MAC allocations that belong to
  no current cluster (these can otherwise exhaust the small allocation range);
- recreates the network if its DHCP range predates the current layout, which
  also flushes stale dynamic leases.

```sh
easyshift nat-network reset --dry-run   # report what would change
easyshift nat-network reset             # apply it
easyshift nat-network reset --force     # also OK to recreate while clusters run
```

Recreating the network briefly drops connectivity for any running NAT cluster,
so `reset` refuses to do it while one is up unless you pass `--force`. The
surviving clusters' reservations are restored automatically.

### `pull-secret` / `dns`

Credential management — see [configuration.md](configuration.md).

```sh
easyshift pull-secret set <file|->     easyshift pull-secret show
easyshift dns set <provider> <file|->  easyshift dns show <provider>
```

## Using the cluster

The admin kubeconfig is written per cluster:

```sh
export KUBECONFIG=~/.config/easyshift/clusters/demo/auth/kubeconfig
oc get nodes
oc get clusterversion
```

NAT-mode clusters are reachable **from the host** (and from each other on the
shared network). Bridge-mode clusters are reachable from anywhere on your LAN.
See [networking.md](networking.md) for the details and tradeoffs.

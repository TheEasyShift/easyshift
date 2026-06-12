[![Build Status](https://github.com/TheEasyShift/easyshift/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/TheEasyShift/easyshift/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/TheEasyShift/easyshift)](https://goreportcard.com/report/github.com/TheEasyShift/easyshift)
[![Go Reference](https://pkg.go.dev/badge/github.com/TheEasyShift/easyshift.svg)](https://pkg.go.dev/github.com/TheEasyShift/easyshift)
[![License](https://img.shields.io/github/license/TheEasyShift/easyshift?color=blue)](LICENSE)

# easyshift

An opinionated OpenShift installer for developers: one command takes your
workstation from nothing to a reachable OpenShift cluster running as local VMs.

At a glance:

| Aspect | What you get |
| --- | --- |
| Host OS | Linux today; macOS planned |
| Privileges | No root — libvirt group access is enough |
| Cluster shape | Always exactly 1 master (multi-master is out of scope by design); single-node today, multiple workers planned |
| NAT networking (default) | Zero-config: magic wildcard DNS (`sslip.io`/`nip.io`), no records to manage; NAT clusters share one L2 segment and can reach each other — good for throwaway clusters and multi-cluster DR topologies |
| Bridge networking | Real LAN IP via an existing host bridge, with manual DNS or automated Cloudflare DNS + Let's Encrypt TLS — for clusters other machines need to reach |
| Lifecycle | Staged, idempotent, resumable: a failed `create` picks up where it stopped, `delete` rolls everything back |

Requirements:

| Requirement | Notes |
| --- | --- |
| Linux host with KVM | CPU virtualization enabled (`vmx`/`svm`) |
| libvirt (`qemu:///system`) | `virsh` and `virt-install` on `PATH`; libvirt-group membership is enough — root is not required |
| CLI tools | `ssh-keygen`, `tar`, `dig` on `PATH` |
| OpenShift pull secret | Fetched via `easyshift pull-secret login` (or download from <https://console.redhat.com/openshift/install/pull-secret>) |

## Install

`easyshift` builds from source. The Makefile is the source of truth (it runs
`go vet` and a gofmt check before building):

```sh
make build      # produces ./easyshift at the repo root
```

For prerequisites, building, and first-run setup in detail, see
**[docs/user/installation.md](docs/user/installation.md)**.

## Quickstart

```sh
# 1. Store your pull secret once (log in to your Red Hat account; or use
#    `easyshift pull-secret set <file>` with a downloaded secret).
easyshift pull-secret login

# 2. Create a zero-config NAT cluster (magic DNS, no records to manage).
easyshift create --name demo

# 3. Check on it / use it.
easyshift status demo
export KUBECONFIG=~/.config/easyshift/clusters/demo/auth/kubeconfig
oc get nodes
```

To configure and use easyshift beyond the quickstart — networking modes, DNS
and TLS automation, and the full command reference — start at the
**[user docs](docs/user/)**.

## Documentation

| For… | Start here |
| --- | --- |
| Running clusters | [docs/user/](docs/user/) — install, configure, use, troubleshoot |
| Contributing / internals | [docs/dev/](docs/dev/) — architecture, stages, providers, testing |

The docs index is at **[docs/README.md](docs/README.md)**.

## Contributing

Contributions are welcome. See **[CONTRIBUTING.md](CONTRIBUTING.md)** for the
quick version and **[docs/dev/contributing.md](docs/dev/contributing.md)** for
the full workflow.

## License

See [LICENSE](LICENSE).

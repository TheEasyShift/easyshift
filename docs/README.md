# easyshift documentation

The docs are split by who you are.

## I want to run clusters → [user/](user/)

| Doc | What it covers |
| --- | --- |
| [installation.md](user/installation.md) | Prerequisites, building the binary, first-run setup |
| [configuration.md](user/configuration.md) | The config dir, `config.json`, pull secret, DNS credentials |
| [usage.md](user/usage.md) | Command reference and the cluster lifecycle |
| [networking.md](user/networking.md) | NAT vs bridge mode, magic DNS, the multi-cluster (DR) story |
| [dns-and-tls.md](user/dns-and-tls.md) | Automated DNS records and Let's Encrypt TLS |
| [access.md](user/access.md) | Accessing your cluster — kubeconfig contexts, console login, `easyshift trust`, TLS |
| [troubleshooting.md](user/troubleshooting.md) | Logs, the `status` command, common failure modes |

A good reading order: **installation → configuration → usage**, then
**networking** when you need more than the zero-config default.

## I want to contribute or understand the internals → [dev/](dev/)

| Doc | What it covers |
| --- | --- |
| [architecture.md](dev/architecture.md) | Package layering and the staged-installer model |
| [stages.md](dev/stages.md) | The stage contract; adding or changing a stage |
| [providers.md](dev/providers.md) | Interfaces, provider implementations, and fakes |
| [image-baking.md](dev/image-baking.md) | `--bake-images`: pre-pulling the release payload into a read-only store |
| [testing.md](dev/testing.md) | Make targets, fakes, and `--simulate` |
| [contributing.md](dev/contributing.md) | Workflow, commit conventions, PR flow |

# GARM provider for Timeweb Cloud

[![CI](https://github.com/burkostya/garm-provider-timeweb/actions/workflows/ci.yml/badge.svg)](https://github.com/burkostya/garm-provider-timeweb/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

Run ephemeral GitHub Actions runners on [Timeweb Cloud](https://timeweb.cloud/) with [GARM](https://github.com/cloudbase/garm). GARM decides when a runner is needed; this external provider creates the VM, passes GARM's bootstrap configuration to cloud-init, and deletes the VM when GARM requests cleanup. Pools and scale sets can scale down to zero runner VMs.

**Status: experimental `v0.1.0` prerelease.** The implementation is covered by automated tests against a local HTTP server. A real Timeweb create → bootstrap → job → delete test is still pending. Use the [smoke-test guide](docs/smoke-test.md) to validate your selected image and networking before moving production jobs.

## Compatibility

| Component | Initial support |
| --- | --- |
| GARM baseline | `v0.2.1`, external interface **`v0.1.0`** |
| Provider executable host | Linux `amd64` or Linux `arm64` |
| Runner guests | Linux **`amd64`** only |
| Image | `os:<positive catalog OS ID>` or `image:<custom image UUID>` |
| Flavor | Positive numeric Timeweb server preset ID |
| Networking | Existing VPC with prepared outbound connectivity; no per-runner public IP allocation |
| Bootstrap | GARM's `garm-provider-common` cloud-init generation, including JIT and template inputs |

Use the explicit interface version below. This release does not implement interface `v0.1.1`. An `arm64` release archive describes the controller host and does not enable `arm64` Timeweb runners. Windows guests are rejected. NixOS custom images need a suitable bootstrap template and have not been tested with this provider.

## 1. Prepare Timeweb and GARM

Before enabling a pool or scale set, prepare:

- A Timeweb API token with permissions to list, create, inspect, start, stop, and delete servers. Unattended deletion must be enabled (`is_able_to_delete=true`); a token that requires SMS or another confirmation cannot complete runner cleanup.
- An existing VPC in the target availability zone, with working outbound access from guests to GitHub, package/download endpoints, and GARM's callback and metadata URLs.
- A Linux `amd64` image with functional cloud-init and a compatible server preset. Select IDs from your current account catalog rather than copying IDs from another account or region.
- A running [GARM `v0.2.1`](https://github.com/cloudbase/garm/releases/tag/v0.2.1) controller and matching `garm-cli`. Follow its [setup](https://github.com/cloudbase/garm/blob/v0.2.1/doc/quickstart-systemd.md) and [first steps](https://github.com/cloudbase/garm/blob/v0.2.1/doc/first-steps.md) to register GitHub credentials and a repository or organization.

See [Timeweb setup and networking](docs/timeweb-setup.md) for catalog lookups, BGP/OVN differences, and bootstrap requirements. In particular, newly created Timeweb VPCs use BGP and do not support the legacy `snat` mode. Prepare routing first; do not set `network_mode="snat"` on a new BGP network. [Timeweb VPC documentation](https://timeweb.cloud/docs/vpc), [NAT documentation](https://timeweb.cloud/docs/vpc/nat).

## 2. Install the provider

Download the archive matching the **GARM host**, check its checksum, and install the executable. For Linux `amd64`:

```bash
version=v0.1.0
archive="garm-provider-timeweb_${version}_linux_amd64.tar.gz"
release_url="https://github.com/burkostya/garm-provider-timeweb/releases/download/${version}"

curl --fail --location --remote-name "${release_url}/${archive}"
curl --fail --location --remote-name "${release_url}/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf "$archive"
sudo install -D -m 0755 garm-provider-timeweb /opt/garm/providers.d/garm-provider-timeweb
/opt/garm/providers.d/garm-provider-timeweb --version
```

For an `arm64` host, use the `_linux_arm64.tar.gz` asset. If GARM runs in a container, place the binary and config at paths visible **inside that container**; this provider is not bundled with the upstream GARM image.

Alternatively, build from source using Go 1.25 or later (CI uses Go 1.26.8):

```bash
make build
sudo install -D -m 0755 bin/garm-provider-timeweb /opt/garm/providers.d/garm-provider-timeweb
```

## 3. Configure the provider

Copy [examples/config.toml](examples/config.toml) to `/etc/garm/timeweb.toml`, replace the network ID, and make the token file readable by the account running GARM:

```toml
token_file = "/etc/garm/timeweb.token"
namespace = "timeweb"
network_id = "network-REPLACE_WITH_YOUR_NETWORK_ID"
```

The token file contains only the token, optionally followed by a newline. Restrict it to the GARM service account, for example with mode `0600`. `token = "..."` is also supported; configure exactly one of `token` and `token_file`. A relative `token_file` is resolved against the directory containing the provider config. Credentials stay on the controller.

| TOML key | Default | Meaning |
| --- | --- | --- |
| `token` / `token_file` | Exactly one required | Inline API token or path to its file |
| `api_url` | `https://api.timeweb.cloud/api/v1` | API base URL; HTTPS is required except for local test servers |
| `namespace` | `timeweb` | Ownership scope; 1–64 letters, digits, `_`, `.`, or `-`, starting with a letter or digit |
| `network_id` | Required | Existing VPC identifier, usually `network-...` |
| `network_mode` | `""` | Leave NAT settings alone. Explicit `snat` or `no_nat` is for compatible legacy OVN networks only |
| `availability_zone` | Omitted | Availability zone compatible with the VPC, image, and preset |
| `project_id` | `0` (omitted) | Existing project ID; positive when set |
| `ssh_key_ids` | `[]` | Existing Timeweb SSH key IDs, if SSH access is needed |
| `request_timeout_seconds` | `60` | Timeout for an API request, from 1 to 600 seconds |

Unknown TOML keys are rejected. An offline check validates syntax, fields, and token-file access without creating resources or checking account permissions:

```bash
/opt/garm/providers.d/garm-provider-timeweb --check-config /etc/garm/timeweb.toml
```

Add [examples/garm-provider.toml](examples/garm-provider.toml) to GARM's own configuration:

```toml
[[provider]]
name = "timeweb"
provider_type = "external"
description = "Ephemeral runners on Timeweb Cloud"

[provider.external]
provider_executable = "/opt/garm/providers.d/garm-provider-timeweb"
config_file = "/etc/garm/timeweb.toml"
interface_version = "v0.1.0"
```

Restart GARM using your service manager, then verify the provider is available:

```bash
garm-cli provider list
```

Keep `namespace` stable while runners exist. Ownership is stored in each server's comment and includes the namespace, GARM controller ID, and pool/scale-set ID. Changing that comment or losing the controller identity prevents normal discovery and cleanup.

## 4. Create an on-demand scale set

After registering your repository in GARM, substitute your selected numeric IDs and repository name:

```bash
TWC_OS_ID='YOUR_NUMERIC_OS_ID'
TWC_PRESET_ID='YOUR_NUMERIC_PRESET_ID'

garm-cli scaleset add \
  --repo your-org/your-repo \
  --name timeweb-amd64 \
  --provider-name timeweb \
  --image "os:${TWC_OS_ID}" \
  --flavor "$TWC_PRESET_ID" \
  --os-type linux \
  --os-arch amd64 \
  --min-idle-runners 0 \
  --max-runners 3 \
  --enabled
```

Target the scale-set name in your repository's workflow:

```yaml
jobs:
  build:
    runs-on: timeweb-amd64
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - run: ./your-build-command
```

`min-idle-runners=0` allows zero idle runner VMs; `max-runners=3` caps that scale set. GARM uses GitHub's scale-set message queue, so a `workflow_job` webhook is not needed for this mode. The controller and shared network infrastructure still need to remain available. See the [GARM scale-set guide](https://github.com/cloudbase/garm/blob/v0.2.1/doc/scale-sets.md).

### Alternative: webhook-driven pool

Install the repository's `workflow_job` webhook as described in [GARM's webhook guide](https://github.com/cloudbase/garm/blob/v0.2.1/doc/webhooks.md), then create a pool:

```bash
garm-cli pool add \
  --repo your-org/your-repo \
  --provider-name timeweb \
  --image "os:${TWC_OS_ID}" \
  --flavor "$TWC_PRESET_ID" \
  --os-type linux \
  --os-arch amd64 \
  --tags timeweb,linux,amd64 \
  --min-idle-runners 0 \
  --max-runners 3 \
  --enabled
```

Use `runs-on: [timeweb, linux, amd64]` for this pool. GARM uses the labels explicitly configured on the pool; it does not automatically add `self-hosted`. See [pool scaling and label matching](https://github.com/cloudbase/garm/blob/v0.2.1/doc/pools-and-scaling.md).

## Per-pool overrides and bootstrap customization

Pools and scale sets can pass these Timeweb fields through `--extra-specs` or `--extra-specs-file`: `network_id`, `network_mode`, `availability_zone`, `project_id`, and `ssh_key_ids`. For example:

```json
{
  "network_id": "network-REPLACE_WITH_YOUR_NETWORK_ID",
  "network_mode": "",
  "ssh_key_ids": []
}
```

See [examples/extra-specs.json](examples/extra-specs.json). Explicit empty `ssh_key_ids` clears the configured key list; empty `network_mode` disables the legacy NAT-mode override. Unknown JSON fields are rejected.

The common GARM fields `runner_install_template`, `pre_install_scripts`, and `extra_context` are also accepted. Script/template byte strings must be base64-encoded in JSON. Pre-install scripts run as root before the runner installation script, but after cloud-init's package stage; they cannot reliably establish the first route needed for package downloads. Prefer a prepared image or early network configuration. Use [GARM runner templates](https://github.com/cloudbase/garm/blob/v0.2.1/doc/templates.md) for custom bootstrap logic and [the common configuration schema](https://github.com/cloudbase/garm-provider-common/blob/v0.1.9/cloudconfig/util.go) for the exact fields.

## Operation and cleanup

The provider implements create, get, list, start, stop, delete, and remove-all operations. Create attempts reconcile an existing owned server with the same runner name before sending another create request. Cleanup can resolve the runner name if the original create response was lost. Missing instances are treated as already deleted; other failures are returned to GARM for recovery.

Managed servers are identified by their ownership comment, not by name prefix alone. Other controllers, namespaces, and unrelated servers are excluded. `RemoveAllInstances` removes managed servers across this controller's pools in the configured namespace. Preserve your GARM database and keep namespace values stable across upgrades.

This version uses existing networks and SSH keys, and requests no per-runner floating IP. Shared routers, NAT addresses, custom images, and other externally managed resources have their own lifecycle and may continue to incur charges after runner VMs scale to zero. Review cleanup in both GARM and Timeweb during the first smoke test.

## Development and releases

```bash
go mod verify
make check
make dist VERSION=v0.1.0
```

`make check` covers formatting, race-enabled Go tests, `go vet`, and a static build. Updating `VERSION` on `main` creates a release at the verified commit; pushing a version tag also runs the release workflow. Releases include Linux `amd64`/`arm64` archives and `SHA256SUMS`. See [releasing](docs/releasing.md), [initial release notes](docs/releases/v0.1.0.md), and [live validation](docs/smoke-test.md).

Licensed under [Apache License 2.0](LICENSE). This is an independent provider; inclusion in GARM's supported-provider table requires a separate upstream contribution.

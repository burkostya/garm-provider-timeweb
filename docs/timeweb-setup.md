# Timeweb setup and networking

The provider manages runner VMs. Prepare the account, VPC, and image before enabling GARM scaling. This guide describes the initial release's supported setup; a real-cloud test is still required for your chosen image and region.

## Account and catalog

Create a Timeweb API token that can manage servers and permits deletion without confirmation. In the API token model this capability is `is_able_to_delete=true`. A deletion response that requires confirmation is reported as a failure, so GARM can retain the runner for cleanup. See the [Timeweb API reference](https://timeweb.cloud/api-docs).

Store the token in a file readable by the GARM service account, set `token_file` in the provider TOML, and validate it with:

```bash
garm-provider-timeweb --check-config /etc/garm/timeweb.toml
```

This check is offline. It does not establish that the token is valid, that the network exists, or that account quotas allow a VM.

Use the Timeweb control panel or these read-only API endpoints with bearer authentication to select current IDs:

| Resource | Endpoint | Response array | Provider input |
| --- | --- | --- | --- |
| Server operating systems | `GET https://api.timeweb.cloud/api/v1/os/servers` | `servers_os` | `--image os:<ID>` |
| Server presets | `GET https://api.timeweb.cloud/api/v1/presets/servers` | `server_presets` | `--flavor <ID>` |
| Private networks | `GET https://api.timeweb.cloud/api/v2/vpcs` | `vpcs` | `network_id` |

The VPC catalog is under API **v2**; keep the provider's `api_url` at the **v1** server API base. Network IDs usually begin with `network-`; they are not custom-image UUIDs. Choose a preset, network, and image available together in your desired availability zone. If assigning a project or SSH keys, use existing numeric IDs from the same account.

For a custom image, pass `--image image:<UUID>`. A valid image ID does not prove that the image can boot or consume Timeweb cloud-init. Start with a cloud-init-enabled Linux `amd64` image and verify the complete bootstrap flow.

## Network setup

The create request contains the existing `network_id` and omits `floating_ip`. This matches the Timeweb CLI's private-only server creation path. The provider does not request a public address for every runner. [Timeweb CLI implementation](https://github.com/timeweb-cloud/twc/blob/c0f56d30d981279d5c0a4b704f180b7daf178b58/twc/commands/server.py).

### BGP VPC: prepare routing first

Timeweb creates new VPCs as BGP networks. The legacy per-server outbound-only NAT mode is unavailable on those networks. Keep `network_mode = ""`, the provider default, and configure an existing router/NAT gateway with appropriate guest routes. Follow [Timeweb's VPC overview](https://timeweb.cloud/docs/vpc) and [virtual-router NAT gateway guide](https://timeweb.cloud/docs/virtual-routers/nat-gateway-setup).

The guest must have working DNS and outbound routing **before cloud-init's package stage**. Bake the required route into the image or provide it through the network's early guest configuration. Merely adding an `ip route` command to GARM's `pre_install_scripts` can be too late: those scripts run in `runcmd`, after package work. The provider does not create routers, manage DHCP, or configure guest network interfaces.

Verify access from a representative guest to GitHub, runner downloads, OS package mirrors, and the controller's callback/metadata URLs. A private controller address requires a route from the VPC. The controller itself needs access to GitHub and the Timeweb API. Scale sets avoid an inbound GitHub webhook requirement, but runner-to-controller connectivity is still needed.

### Existing OVN VPC: optional legacy SNAT

For an existing OVN network with NAT already prepared, `network_mode = "snat"` requests outbound-only connectivity. `no_nat` requests private-network-only mode. The provider applies the selected legacy mode after VM creation and attempts to clean up a newly created VM if that configuration fails.

Timeweb may enable shared NAT and assign a gateway address when SNAT is enabled. Prepare those shared resources explicitly and account for them separately; the provider does not remove them. An allocated gateway address can remain billable after runner deletion. See [Timeweb's NAT behavior and billing](https://timeweb.cloud/docs/vpc/nat).

The initial release does not offer `dnat_and_snat` or a per-runner public-IP allocation option. Use prepared VPC egress for runners.

## Bootstrap and images

The provider passes GARM's generated cloud-config to Timeweb's `cloud_init` field. It carries the runner installation script, GARM callback/metadata information, SSH keys, CA bundle, and supported customization. Timeweb documents the cloud-init log at `/var/log/cloud-init-output.log`; custom images need their own cloud-init validation. [Timeweb cloud-init guide](https://timeweb.cloud/docs/cloud-servers/manage-servers/cloud-init).

For a standard Linux image, start with GARM's normal GitHub Linux template. Use a custom image for project dependencies that would otherwise extend cold-start time. Use [GARM's template mechanism](https://github.com/cloudbase/garm/blob/v0.2.1/doc/templates.md) for OS-specific installation logic, including a potential NixOS image. NixOS support is not verified by this release.

Supported common extra-spec fields are `runner_install_template`, `pre_install_scripts`, and `extra_context`. Template/script values are base64 strings in JSON; script map keys become filenames, and scripts execute alphabetically. They run as root, so treat the pool's extra specs as controller configuration. See the [pinned common schema](https://github.com/cloudbase/garm-provider-common/blob/v0.1.9/cloudconfig/util.go).

The Timeweb token is used by the provider process on the controller and is not placed in runner cloud-init. GARM's runner bootstrap credentials are necessarily part of the guest bootstrap. Avoid publishing raw cloud-init, token files, or unredacted bootstrap logs when reporting a problem.

## Ownership and recovery

The server comment stores the provider identity, namespace, controller ID, and pool/scale-set ID. Keep that comment intact and preserve GARM's database. Changing the namespace while VMs exist makes them belong to a different scope, so remove old runners before changing it.

An interrupted create can leave a VM whose numeric ID never reached GARM. The provider resolves owned servers by runner name for recovery. If several owned VMs have the same name, it reports ambiguity rather than selecting one. Inspect the matching numeric IDs and their comments before manual cleanup.

A successful deletion request is not a substitute for the first real-cloud cleanup check. Verify that the VM disappears from Timeweb, and separately inspect any shared gateways, public IPs, images, or backups you created outside this provider. Continue with the [smoke-test guide](smoke-test.md).

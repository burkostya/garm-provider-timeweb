# Live smoke test

**Release status: pending.** Automated tests exercise the Timeweb API contract and provider logic with a local HTTP server. They do not prove that Timeweb delivers the selected image's cloud-init or that a real runner joins GitHub. This checklist records the first complete cloud validation.

Run this in a dedicated test repository and namespace with a single runner maximum. Creating a runner VM and any required shared infrastructure uses your Timeweb account and can incur charges.

## Record the environment

Record the following without including secrets:

| Field | Value |
| --- | --- |
| Date and operator | Pending |
| Provider version and commit (`--version`) | Pending |
| GARM version | Pending |
| Controller architecture | Pending |
| Guest image and OS version | Pending |
| Preset ID and availability zone | Pending |
| Network type and egress method | Pending |
| Mode: scale set or pool | Pending |
| Job URL and runner/server IDs | Pending |
| Create, join, job-finish, deletion timestamps | Pending |
| Result and cleanup evidence | Pending |

## Prepare one scale set

1. Complete [Timeweb setup](timeweb-setup.md), including unattended deletion permission and early guest egress.
2. Run `garm-provider-timeweb --check-config /etc/garm/timeweb.toml` as the GARM service account. Confirm the provider appears in `garm-cli provider list`.
3. Register the test repository and credentials in GARM. Create the scale set from the README with `--name timeweb-smoke`, `--min-idle-runners 0`, and **`--max-runners 1`**.
4. Record the pre-test Timeweb server and shared-network resource inventory. Confirm there are no runner VMs for this test namespace while the scale set has no jobs.

Add this workflow to the test repository, then dispatch it once:

```yaml
name: Timeweb smoke test

on:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  smoke:
    runs-on: timeweb-smoke
    timeout-minutes: 10
    steps:
      - name: Verify runner platform
        shell: bash
        run: |
          set -euo pipefail
          test "$(uname -s)" = Linux
          test "$(uname -m)" = x86_64
          printf 'Runner: %s\n' "$RUNNER_NAME"
          uname -a
```

## Observe the complete lifecycle

- [ ] The queued job causes exactly one new VM in the expected VPC, zone, and project.
- [ ] The server comment contains the test namespace, correct controller, and scale-set ownership.
- [ ] The VM has the intended private network configuration, with no unexpected per-runner public IP.
- [ ] Cloud-init completes and the runner joins the intended GitHub repository.
- [ ] The dispatched job runs successfully on Linux `amd64`.
- [ ] After the job, GARM removes the ephemeral runner and the Timeweb VM disappears.
- [ ] With no queued jobs, the scale set returns to zero runner VMs.
- [ ] Shared networking remains in its expected state, with no unexpected new IP, disk, backup, or other resource.

Watch progress with `garm-cli scaleset list`, `garm-cli scaleset runner list <SCALESET_ID>`, and `garm-cli runner show <RUNNER_NAME>`. Inspect the Timeweb console and cloud-init log if registration fails. Preserve redacted errors and timestamps before cleanup.

## Repeat and check recovery

- [ ] Dispatch two short jobs while the maximum is one. Confirm the second queues and the account never exceeds the test's one-runner limit.
- [ ] Run a job again after scale-to-zero and confirm it gets a new VM.
- [ ] In this isolated test only, cancel a job during startup. Confirm GARM eventually cleans up the VM through normal failure recovery.
- [ ] Confirm an unrelated VM or a VM in a different provider namespace stays untouched throughout the test.

For webhook-driven pools, repeat the basic lifecycle using `--tags timeweb-smoke`, `runs-on: timeweb-smoke`, and a correctly installed `workflow_job` webhook. Record the two modes separately; success in one mode is not a recorded test of the other.

## Finish cleanup

Disable the test scale set with `garm-cli scaleset update <SCALESET_ID> --enabled=false`. Let an active job finish or cancel it, then remove remaining test runners through GARM. Check Timeweb directly and confirm that every recorded runner VM has disappeared. A stopped VM has not been deleted.

If cleanup reports deletion confirmation or token-permission errors, fix the token and retry. Avoid forcing GARM to forget a runner while its VM still exists. For a manual recovery, identify the VM by its numeric ID and ownership comment, and verify deletion afterward.

Delete the empty test scale set, and remove test-only shared infrastructure separately when it is no longer needed. Fill in the environment table with the result and evidence before promoting the provider to a production-ready release. Keep the initial release marked experimental if any create, bootstrap, job, or deletion step remains unverified.

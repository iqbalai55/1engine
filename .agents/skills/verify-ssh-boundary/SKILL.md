---
name: verify-ssh-boundary
description: Use when a change touches the dstack proxy's run plan or response boundary, especially SSH key handling, host address scrubbing, or tenant scoping. Runs Go fuzz targets that check the secure SSH property on the real functions.
---

# Verify the secure SSH boundary

## What this checks

Two properties on the real code:

1. A run plan forwarded to dstack never contains a customer-supplied `ssh_key_pub`.
   dstack fills the key from the tenant user's server-managed key, so a customer never holds
   a key that opens a host.
2. A JSON response returned to a customer never contains `job_provisioning_data`,
   `attached_ssh_command`, `attached_ide_url`, or a hostname on an instance object. The
   proxied SSH command is the only connection detail that survives.

A break in the first property lets a customer install their own key on a host. A break in the
second hands a customer the host address. Both defeat the product boundary.

## Tool choice

Go has no practical verifier for these functions. The checks are native Go fuzz targets on
`internal/dstack`. They cover every input the fuzzer reaches within `FUZZTIME`, not a proof
over all inputs. `FUZZTIME` defaults to `30s` per target.

## Coverage

- `internal/dstack/proxy.go`: `bindProviderBackends` and `scrubSSHAccess`/`redactJSON`.
- `internal/dstack/ssh_boundary_fuzz_test.go`: the two targets and their assertions.
- `scripts/verify-ssh-boundary.sh`: the runner.
- Sync commit: `3d86ff0326f3483268271cfe9fe30c508874e6cd`. The verification describes the
  code at this commit. Move it forward only after you confirm the targets still describe the
  changed code.

## Run it

```sh
./scripts/verify-ssh-boundary.sh
```

The script uses the local Go toolchain when `go` is installed. Otherwise it runs
`golang:1.26.11-alpine` in Docker. For a quick check:

```sh
FUZZTIME=5s ./scripts/verify-ssh-boundary.sh
```

For a deeper run:

```sh
FUZZTIME=5m ./scripts/verify-ssh-boundary.sh
```

## Read a failure

The failure names the target, the input, and the forwarded or returned JSON.

- `FuzzRunPlanStripsCustomerSSHKey`: the output contains `ssh_key_pub`. The guard is
  `delete(spec, "ssh_key_pub")` in `bindProviderBackends`. Restore it or explain why the
  plan should carry a key. Do not weaken the assertion.
- `FuzzResponseScrubsDirectHostAccess`: the output contains `job_provisioning_data`,
  `attached_ssh_command`, `attached_ide_url`, or an instance `hostname`. The guard is
  `scrubSSHAccess`. Restore it or explain why the customer needs the field. Do not weaken
  the assertion.

If the fuzzer finds a counterexample, Go writes it under `$GOCACHE/fuzz`. Rerun the target
to replay it.

## Proof the checks can fail

Removing `delete(spec, "ssh_key_pub")` makes `FuzzRunPlanStripsCustomerSSHKey` fail on seed
0 and seed 1. Removing `delete(v, "job_provisioning_data")` makes
`FuzzResponseScrubsDirectHostAccess` fail on seed 1 and seed 3. Restore the guard before
handoff.

## When the boundary changes on purpose

If a new field carries host access, add it to `directHostAccess` and add a seed. If a new
response path can carry the proxied command, add a seed and keep the preservation assertion
in `TestProxyScrubsDirectHostAccessFromResponses`. Then run both targets and move the sync
commit to the new code commit.

Do not change `internal/dstack/proxy.go` to make a failing target pass unless the product
decision changed. Report the counterexample first.

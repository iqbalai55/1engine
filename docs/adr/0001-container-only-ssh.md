# Container-only SSH for customers

## Status

accepted

## Context

Customers rent GPU capacity, not a host. dstack's default run spec accepts a
customer-supplied SSH public key and installs it on both the run container and the host. That
gives a customer host access to the operator's VM, the dstack agent, and the cloud identity
attached to the instance. dstack's tenant isolation guide treats host SSH as a configuration
choice, not a security boundary, so the product has to enforce the boundary itself.

## Decision

All customer SSH goes through `dstack-sshproxy` with
`DSTACK_SERVER_SSHPROXY_ENFORCED=1`. The backend strips `run_spec.ssh_key_pub`, removes
direct host addresses from proxied responses, registers customer public keys through
`/v1/ssh-keys`, and returns only the proxied command through `/v1/runs/{name}/ssh`. The proxy
port binds to loopback, and a TCP load balancer exposes it publicly.

## Consequences

- Customers get a shell inside their own run container and never the host.
- The proxy is a public SSH endpoint and a single point of failure. It needs key-only
  authentication, rate limiting, host key rotation, and a redundant deployment if
  availability matters.
- The operator remains the trusted party that can see data on the host. The terms must say
  so.
- Host networking, privileged mode, and instance volumes are separate escape paths and must
  be blocked independently.

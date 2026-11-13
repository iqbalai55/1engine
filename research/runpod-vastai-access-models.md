# How RunPod and Vast.ai scope customer access

Research question: when a customer rents a GPU from RunPod or Vast.ai, do they get a
container or the host machine, and how is SSH handled?

## Summary

Both sell **containers**, not virtual machines and not host access. The customer gets root
inside their own container and SSH into that container. The physical host, its operating
system, its cloud identity, and its other tenants stay with the platform or the host owner.
SSH keys are customer-supplied public keys, registered on the account and injected into the
container at creation. Neither product gives the renter a shell on the host.

## RunPod

- A Pod is a Docker container. RunPod runs Docker for the customer: "Docker Compose is not
  supported: Runpod runs Docker for you, so you cannot spin up your own Docker instance"
  ([Pods overview](https://docs.runpod.io/pods/overview)).
- SSH lands in the Pod. Two modes: basic SSH "proxied through Runpod's systems", or full SSH
  over a public IP with a mapped port. Both use key authentication
  ([Connect with SSH](https://docs.runpod.io/pods/configuration/use-ssh)).
- Keys are account-level public keys, injected into every Pod, and injected dynamically if
  added while a Pod is running. A per-Pod `SSH_PUBLIC_KEY` overrides the account keys
  ([Credentials](https://docs.runpod.io/get-started/credentials)).
- RunPod distinguishes Secure Cloud (T3/T4 partner data centers) from Community Cloud
  (vetted peer-to-peer hosts). For sensitive data it recommends Secure Cloud. Its FAQ says
  hosts are prohibited by the Terms of Service from inspecting Pod data, but it still points
  sensitive workloads at Secure Cloud
  ([FAQ](https://docs.runpod.io/references/faq)).
- The customer never receives host credentials. The Pod is the rental unit.

## Vast.ai

- "Each rental contract is associated with one or more instances: currently implemented via
  docker containers" ([Overview](https://vast.ai/docs/introduction/overview)).
- "You cannot run docker in our instances because they are docker containers themselves"
  ([Instances FAQ](https://vast.ai/docs/console/instances)).
- SSH is key-only: "Vast.ai instances are configured to accept keys only - Password
  authentication is disabled." The customer connects as `root` inside the container over a
  direct port or a proxy ([SSH Connection](https://docs.vast.ai/guides/instances/connect/ssh)).
- Keys are registered on the account before instance creation and applied when the container
  is created ([Keys](https://docs.vast.ai/guides/reference/keys)).
- Vast.ai is explicit that the host owner can reach the data: "Hosts can generally see or
  access your files. They're not allowed to do so, but we have no way of making it impossible
  for them to see things that are on machines they physically control. If you have sensitive
  data, we recommend using a datacenter"
  ([Instances FAQ](https://vast.ai/docs/console/instances)).
- The renter gets container root, not host root. The host owner retains the machine.

## Comparison

| Dimension | RunPod | Vast.ai | 1engine today |
| --- | --- | --- | --- |
| Rental unit | Docker Pod | Docker container | dstack run container |
| Customer root | Inside the Pod | Inside the container | Inside the run container |
| SSH target | Pod, proxied or public IP | Container, direct or proxy | Not exposed |
| Key model | Account public keys, injected | Account public keys, injected | Run-spec key or server-managed key |
| Host access for customer | No | No | Possible through the run-spec key |
| Host trust | Platform or vetted host; ToS restricts inspection | Host can see files; datacenter recommended | Operator owns the host |
| Isolation | Container | Container | Container |

## What this means for 1engine

1. Selling SSH is normal in this market. Both competitors include it.
2. Selling host access is not normal. Neither competitor does it.
3. The standard mechanism is a key registry plus a proxy or mapped port into the container.
4. The host remains the provider's asset. On Vast.ai the host owner is a third party and can
   see the container's files; on RunPod the host is the platform or a vetted partner. 1engine
   is in the RunPod Secure Cloud position: the operator owns the host and must be the trusted
   party.
5. The product boundary is the container. That matches the dstack SSH proxy design with
   `DSTACK_SERVER_SSHPROXY_ENFORCED=1`.

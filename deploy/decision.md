# Docker deployment decision

## Problem

The Go backend requires an HTTPS `DSTACK_URL`. The official dstack image listens on HTTP port 3000. The deployment must provide PostgreSQL, persistent state, secrets, and a trusted HTTPS route without weakening the backend's credential transport.

## Usage

```sh
cd backend/deploy
cp .env.example .env
# Replace every placeholder with generated secrets and real Midtrans and SMTP credentials.
./validate.sh
docker compose up -d
```

The operator reads `README.md` for secret generation, bootstrap, backups, and port details. Docker Compose starts the backend and dstack control plane. The backend connects to dstack at `https://dstack-edge` and keeps its existing dstack client behavior.

## Shape

The service graph has one PostgreSQL server with separate `billing` and `dstack` databases and roles, a pinned dstack server, an SSH proxy for customer run access, a Caddy TLS endpoint, the Go backend, and a one-shot certificate initializer. Named volumes hold database state, dstack's server directory, the private CA, trust bundle, service certificates, and the SSH proxy host keys.

The cert initializer creates an internal CA, leaf certificates, and SSH proxy host keys. Caddy terminates TLS for dstack. The backend trusts that CA while retaining public roots for Midtrans and SMTP. The PostgreSQL server requires TLS and serves both application databases under distinct credentials. Published ports bind to host loopback only. The admin token must match between dstack and the backend and persists after first boot.

dstack runs with the SSH proxy enforced. The proxy is the only path to a customer container, and the customer's key never reaches the host. The Compose stack keeps the proxy on loopback; a TCP load balancer exposes it to customers.

The design follows **Foundational Thinking** by first tracing the deployment data shape, then defining the Compose service graph and persistent volumes before wiring startup logic. This changed the work from a backend HTTP-mode change to a deployment-level TLS endpoint.

The design follows **Model the Domain** by treating deployment as a service graph with named trust and state volumes. This changed the Compose configuration to give the private CA key, public trust bundle, dstack certificate, and PostgreSQL certificate distinct volume ownership and read-only consumers.

The design follows **Boundary Discipline** by keeping HTTP/TLS enforcement at the existing backend and network boundaries. This avoided threading a plaintext opt-in through four independent HTTPS checks.

The design follows **Laziness Protocol** by reusing one PostgreSQL instance with two databases and roles rather than creating separate Postgres containers. This reduced the deployment footprint while retaining independent schemas and credentials.

The design follows **Experience First** by keeping the operator flow to an env file, one validation script, and `docker compose up`. It also makes the no-compute-backend limitation explicit, so operators do not mistake a healthy dstack control plane for configured GPU capacity.

## Alternatives considered

- An HTTP opt-in in the backend was rejected because it weakens a fail-closed credential path and requires code changes in configuration, provisioning, observation polling, and proxy requests.
- Two PostgreSQL containers were rejected because one instance can host the separate databases and roles while the deployment stays smaller.
- Separate PostgreSQL databases were chosen over separate schemas because dstack and the backend both own a `users` table and have independent migrations.
- Public dstack publishing was rejected. The dstack endpoint and backend port bind only to loopback. Operators can place their own public TLS proxy in front of the backend.

## Tradeoffs and risks

- The operator must keep the dstack and backend admin-token values aligned. Changing the env value does not rotate dstack's persisted token.
- dstack has no compute backend in the supplied configuration. Operators must configure a cloud, Kubernetes, or SSH backend before workloads can run.
- The SSH proxy is a public endpoint once the operator exposes it. It binds to loopback by default, requires a TCP load balancer, and becomes a single point of failure. Run a redundant pair if availability matters.
- The backend itself listens on HTTP and binds to loopback. A separate TLS reverse proxy is required for public billing traffic.
- The Compose stack uses named volumes. Removing them deletes persistent data and generated certificate material.
- PostgreSQL TLS terminates at the shared private network server. It protects client connections, but the administrative CA and certificate secrets remain local files in volumes.

## Verification

`deploy/validate.sh` renders Compose, checks loopback port binding and volume ownership, builds both images with unique validation tags, and verifies that the certificate initializer creates signed certificates and trust material. A live run on isolated host ports brought up Postgres, dstack, Caddy, and the backend as healthy services. The backend health route returned `{"status":"ok"}`. The dstack HTTPS endpoint returned `{"status":"running"}`. PostgreSQL reported TLS for both application roles. Through the live backend, a pending tenant reached `ready` and received a stored credential. The provider bootstrap also succeeded after a verified test account was created.

`go test ./...` and `go vet ./...` passed in `backend/`. The bootstrap command also passed after a verified portal user was seeded in the test database. The first bootstrap attempt correctly rejected an email with no verified account.

## Synthesis decision

Candidate B was the base because it tested an unchanged Go backend against a real dstack container behind Caddy and used separate databases in one PostgreSQL instance. Candidate A's second database container and tenant-backend provisioning changes were rejected as extra operational and product scope. The candidate cross-judge timed out. The parent reviewed both candidates and checked the current dstack docs and release metadata directly.

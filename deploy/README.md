# Self-hosted dstack deployment

This directory runs dstack next to the billing backend with Docker Compose. A private
certificate authority signs the dstack and PostgreSQL certificates, and Caddy terminates
TLS in front of dstack. The backend reaches dstack over that TLS endpoint, so the backend
needs no code change.

The dstack server in this deployment has no compute backend. It stores projects, users, and
runs, but it cannot start a GPU workload until an operator adds a cloud, Kubernetes, or SSH
backend. See "Give dstack a compute backend".

## What runs

| Service | Image | Role |
| --- | --- | --- |
| `postgres` | `postgres:16` | One server that holds the `billing` and `dstack` databases, each with its own role |
| `dstack` | `dstackai/dstack:0.22.7` | dstack server, private HTTP port 3000 |
| `caddy` | `caddy:2.11-alpine` | TLS endpoint for dstack, network alias `dstack-edge` |
| `backend` | built from `..` | Billing API, HTTP port 8080 |
| `cert-init` | built from `certs/` | One-shot job that creates the certificate authority, the two server certificates, the trust bundle, and the dstack server config |
| `bootstrap` | built from `..` | One-shot job that promotes a portal account to provider administrator |

## Prerequisites

- Docker with the Compose plugin.
- A host that can reach Docker Hub, because the build pulls the Go and Alpine base images.
- Python 3 for `validate.sh`.

## Create the environment file

Copy the template and generate every secret. Hex keeps the values safe inside a
PostgreSQL URL.

```sh
cd backend/deploy
cp .env.example .env
```

```sh
openssl rand -hex 24   # POSTGRES_SUPERUSER_PASSWORD
openssl rand -hex 24   # BILLING_DB_PASSWORD
openssl rand -hex 24   # DSTACK_DB_PASSWORD
openssl rand -hex 24   # DSTACK_ADMIN_TOKEN
openssl rand -hex 32   # SESSION_SECRET
openssl rand -hex 16   # DSTACK_ENCRYPTION_KEY, exactly 32 bytes
```

Put those values in `.env`. Then replace the remaining placeholders with the real
Midtrans keys, the SMTP credentials, the public address of the backend, and the
provider account email. `PUBLIC_BASE_URL` must be an absolute `https` URL because it
becomes the link in every mail the backend sends.

The backend listens on plain HTTP. Put a TLS reverse proxy in front of the published
backend port before any public traffic reaches it.

## Validate the deployment

```sh
./validate.sh
```

The script renders the Compose model against `.env.example`, checks that every published
port stays on loopback, builds the backend and certificate images, and runs the
certificate image against throwaway volumes to confirm that it produces a certificate
authority, two signed certificates, and a trust bundle. It uses throwaway volumes and
unique image tags, so it does not alter deployment state or existing image tags.

## Start the stack

```sh
docker compose up -d
docker compose ps
```

The `cert-init` job runs once and exits. Everything else stays up and restarts on
failure.

## Bootstrap the provider account

Create and verify a portal account in the running backend first. Then run the one-shot
job with the same email address.

```sh
docker compose --profile bootstrap run --rm bootstrap
```

The database allows one provider account. The command fails after the first successful
bootstrap.

## Reach dstack from the operator CLI

The dstack CLI verifies the server certificate with `requests`, so it needs the trust
bundle. Copy the bundle out of the trust volume.

```sh
docker compose run --rm --no-deps --entrypoint cat cert-init /trust/ca-bundle.pem > ca-bundle.pem
```

Then install the CLI and point it at the published port.

```sh
pip install dstack==0.22.7
export DSTACK_ADMIN_TOKEN=$(grep '^DSTACK_ADMIN_TOKEN=' .env | cut -d= -f2)
export DSTACK_HTTPS_PORT=$(grep '^DSTACK_HTTPS_PORT=' .env | cut -d= -f2)
export REQUESTS_CA_BUNDLE="$PWD/ca-bundle.pem"
dstack project add --name main --url "https://localhost:${DSTACK_HTTPS_PORT}" --token "$DSTACK_ADMIN_TOKEN"
```

The CLI stores the project in `~/.dstack/config.yml` on the host.

The dstack web UI is served on the same port. Open `https://localhost:${DSTACK_HTTPS_PORT}`
in a browser that trusts the certificate authority.

## Give dstack a compute backend

A dstack server without a backend has no compute. Add one as an administrator, either in
the web UI or with the HTTP API.

```sh
curl --cacert ca-bundle.pem -X POST "https://localhost:${DSTACK_HTTPS_PORT}/api/project/main/backends/create_yaml" \
  -H "Authorization: Bearer $DSTACK_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d @backend.yml
```

The deployment does not inject a backend into any tenant project. You own that file and
its cloud credentials.

## Ports

| Port | Host binding | Service |
| --- | --- | --- |
| 8080 | `127.0.0.1` | Billing backend HTTP |
| 8443 | `127.0.0.1` | dstack over TLS, for the operator CLI and the web UI |
| 3000 | not published | dstack server, reachable only inside the Compose network |
| 5432 | not published | PostgreSQL, reachable only inside the Compose network |

Change the two published ports with `BACKEND_HTTP_PORT` and `DSTACK_HTTPS_PORT` in
`.env`. Both stay bound to loopback.

## Logs

```sh
docker compose logs -f backend
docker compose logs -f dstack
docker compose logs -f caddy
```

## Backups

Dump both databases from the running server.

```sh
docker compose exec postgres pg_dump -U postgres --format=custom billing > billing.dump
docker compose exec postgres pg_dump -U postgres --format=custom dstack > dstack.dump
```

Back up the dstack server directory as well. It holds `config.yml`, which carries the
AES key that encrypts secrets stored in the dstack database. A dump without that key
cannot be restored.

```sh
docker run --rm -v 1inference_dstack-data:/data -v "$PWD:/backup" alpine:3.21 \
  tar czf /backup/dstack-data.tar.gz -C /data .
```

Back up the trust volume too. The certificate authority lives in `1inference_ca-keys` and
only the `cert-init` container mounts it. Losing it means reissuing every certificate.

```sh
docker run --rm -v 1inference_ca-keys:/data -v "$PWD:/backup" alpine:3.21 \
  tar czf /backup/ca-keys.tar.gz -C /data .
```

## The admin token and the first start

dstack reads `DSTACK_SERVER_ADMIN_TOKEN` only when it creates the admin user. It stores
the token in the database on that first start and ignores the variable afterwards.

Two consequences. A later change to `DSTACK_ADMIN_TOKEN` in `.env` does not change the
token that dstack accepts, and the backend keeps using the value from `.env` for the
admin API, so the two must stay in step. The PostgreSQL initialisation script also runs
only against an empty data directory, so a changed database password needs either a
matching `ALTER ROLE` or a fresh volume.

The `cert-init` job follows the same rule. It creates the certificate authority, the
certificates, the trust bundle, and `config.yml` only when they are missing. Deleting a
volume makes the next start issue new material, and a new certificate authority
invalidates every leaf certificate that the old one signed.

## What this deployment does not include

- No compute backend. dstack starts without one.
- No SSH proxy. The dstack SSH fleet port is not published.
- No public dstack port. The TLS endpoint is bound to loopback.
- No frontend container. The dstack UI ships inside the dstack image, and the billing
  frontend is served separately.
- No tenant backend injection. The backend creates dstack users and projects, but it
  never writes backend configuration into a project.

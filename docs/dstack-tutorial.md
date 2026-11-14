# Tutorial: run dstack with AWS, GCP, Nebius, and RunPod

This tutorial sets up the self-hosted stack in `deploy/` and connects four compute providers
(AWS, GCP, Nebius, RunPod) to it. When you finish, customers can call the billing backend,
and the backend forwards their dstack requests to your dstack server using only the providers
you configured.

## How the pieces fit

```mermaid
flowchart LR
  C[Customer with API key] -->|/v1/api/...| B[Go billing backend]
  B -->|tenant token over TLS| D[Self-hosted dstack]
  D --> P[AWS, GCP, Nebius, RunPod]
  F[provider-backends.yml] -->|read at startup| B
  F -->|create_yaml / update_yaml per tenant project| D
```

- **Go backend** (`internal/`): checks the API key, checks credits, and forwards the request.
  It also limits fleet and run plans to the providers in your YAML file.
- **dstack server** (`deploy/compose.yaml`, service `dstack`): stores projects, runs, and
  fleets. It starts GPU workloads only after a cloud backend is configured.
- **Caddy** (service `caddy`): terminates TLS in front of dstack with a private CA.
- **provider-backends.yml**: your cloud credentials. The backend writes each entry into every
  tenant's dstack project, and it re-applies them every minute.

## 1. Prerequisites

- Docker with the Compose plugin.
- Python 3, for `deploy/validate.sh`.
- `openssl`, to generate secrets.
- Accounts and credentials for the providers you want (step 3).
- A verified portal account for the provider administrator. The backend must be running
  before you can create it (step 6).

Run every command below from the repository root, `1engine/`, unless it says otherwise.

## 2. Create the environment file

```sh
cd deploy
cp .env.example .env
```

Generate each secret and paste it into `.env`:

```sh
openssl rand -hex 24   # POSTGRES_SUPERUSER_PASSWORD
openssl rand -hex 24   # BILLING_DB_PASSWORD
openssl rand -hex 24   # DSTACK_DB_PASSWORD
openssl rand -hex 24   # DSTACK_ADMIN_TOKEN
openssl rand -hex 24   # SSHPROXY_API_TOKEN
openssl rand -hex 32   # SESSION_SECRET
openssl rand -hex 16   # DSTACK_ENCRYPTION_KEY (must be exactly 32 bytes)
```

Then set the remaining values:

| Variable | What to put |
| --- | --- |
| `MIDTRANS_SERVER_KEY`, `MIDTRANS_CLIENT_KEY` | Midtrans keys. Use sandbox keys while testing. |
| `PUBLIC_BASE_URL` | Public HTTPS address of the backend, for example `https://billing.example.com`. |
| `SSHPROXY_PUBLIC_ADDRESS` | Public `HOST:PORT` customers use for SSH, for example `ssh.example.com:2222`. |
| `SMTP_HOST`, `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_FROM` | Mail server used for verification emails. Port 465 (implicit TLS) is required. |
| `PROVIDER_ADMIN_EMAIL` | The email of the portal account that becomes provider administrator. |

Do not commit `deploy/.env`.

## 3. Get credentials for each provider

Create one credential per provider. The fields below are the ones the backend parser accepts.
dstack itself validates the values when it applies them, so check the dstack backend docs
for the permissions each credential needs.

| Provider | What you need |
| --- | --- |
| AWS | An access key ID and secret access key for an IAM user that can launch EC2 instances. |
| GCP | The project ID and a service account JSON key with Compute Engine permissions. |
| Nebius | A service account ID, a public key ID, and the matching private key (PEM). |
| RunPod | An API key. |

Keep these values in a password manager or your secret store. You paste them into
`provider-backends.yml` in the next step.

## 4. Write provider-backends.yml

Create `deploy/provider-backends.yml`. Use one entry per provider. Each `type` may appear only
once, and only `aws`, `gcp`, `nebius`, `runpod`, and `vastai` are accepted. Replace every placeholder:

```yaml
backends:
  - type: aws
    creds:
      type: access_key
      access_key: AKIA...
      secret_key: ...

  - type: gcp
    project_id: your-gcp-project-id
    creds:
      type: service_account
      data: '{"type":"service_account","project_id":"...","private_key":"...", ...}'

  - type: nebius
    creds:
      type: service_account
      service_account_id: ...
      public_key_id: ...
      private_key_content: |
        -----BEGIN PRIVATE KEY-----
        ...
        -----END PRIVATE KEY-----

  - type: runpod
    creds:
      type: api_key
      api_key: ...
```

You can leave out a provider you do not use. The backend then offers only the providers that
remain. Lock down the file:

```sh
chmod 600 deploy/provider-backends.yml
```

The repository's `.gitignore` already lists `provider-backends.yml`, and `deploy/*.pem` covers the
trust bundle. Keep both files out of git.

## 5. Mount the file into the backend

`deploy/compose.yaml` already sets `PROVIDER_BACKENDS_FILE` on the `backend` service and mounts
`deploy/provider-backends.yml` read-only at `/config/provider-backends.yml`. The file must exist
before you start the stack. The mount uses `create_host_path: false`, so Compose stops with an
error instead of creating an empty directory at that path.

Check the mount before you start:

```sh
docker compose config --format json | python3 -c "import json,sys; print(json.load(sys.stdin)['services']['backend']['volumes'])"
```

Without the file, the backend has no providers, and no provider reaches any tenant project.

## 6. Validate and start the stack

```sh
./validate.sh
docker compose up -d
docker compose ps
```

Wait until `postgres`, `dstack`, `caddy`, and `backend` show `healthy`. The `cert-init` job
runs once and exits. Check the logs if a service keeps restarting:

```sh
docker compose logs -f backend
docker compose logs -f dstack
```

A line like `provider backend sync failed` means dstack rejected one of your credentials.
Fix the value in `provider-backends.yml` and restart the backend:

```sh
docker compose restart backend
```

Check that the backend answers:

```sh
curl http://127.0.0.1:8080/v1/health
# {"status":"ok"}
```

## 7. Create the provider administrator

1. Open your backend's public address, sign up with the email in `PROVIDER_ADMIN_EMAIL`, and
   verify the address from the email you receive.
2. Run the one-shot bootstrap job:

   ```sh
   docker compose --profile bootstrap run --rm bootstrap
   ```

   You should see `provider admin bootstrapped`. The database allows only one provider account,
   so the command fails if you run it again.

## 8. Connect the dstack CLI to your server

The dstack server is reachable only through Caddy on loopback. Copy the trust bundle out of the
volume, then point the CLI at it:

```sh
docker compose run --rm --no-deps --entrypoint cat cert-init /trust/ca-bundle.pem > deploy/ca-bundle.pem

pip install dstack==0.22.7
export DSTACK_ADMIN_TOKEN=$(grep '^DSTACK_ADMIN_TOKEN=' deploy/.env | cut -d= -f2)
export DSTACK_HTTPS_PORT=$(grep '^DSTACK_HTTPS_PORT=' deploy/.env | cut -d= -f2)
export REQUESTS_CA_BUNDLE="$PWD/deploy/ca-bundle.pem"
dstack project add --name main --url "https://localhost:${DSTACK_HTTPS_PORT}" --token "$DSTACK_ADMIN_TOKEN"
```

Open `https://localhost:${DSTACK_HTTPS_PORT}` in a browser that trusts the CA to see the web UI.

Do not commit `deploy/ca-bundle.pem`.

## 9. Confirm the providers reached a tenant project

Each new tenant gets a dstack project when it finishes provisioning. Your four backends are
written into that project automatically. To check one:

1. Open the dstack web UI and select the tenant's project.
2. Go to project settings, then backends. You should see the types you configured in step 4.

Or query the backend directly, replacing `PROJECT` with the project name:

```sh
curl --cacert deploy/ca-bundle.pem -X POST \
  "https://localhost:${DSTACK_HTTPS_PORT}/api/project/PROJECT/backends/aws/get_yaml" \
  -H "Authorization: Bearer $DSTACK_ADMIN_TOKEN" \
  -H "Content-Type: application/json" -d '{}'
```

Change `aws` to `gcp`, `nebius`, `runpod`, or `vastai` for the others. The response contains the stored
config, including credentials, so do not paste it anywhere.

## 10. Give a customer access

A customer does three things:

1. Signs up and verifies the email address on the portal.
2. Creates an API key with `POST /v1/api-keys` while logged in.
3. Calls dstack through the allowlisted routes with that key:

   ```sh
   curl -X POST https://billing.example.com/v1/api/runs/list \
     -H "Authorization: Bearer $CUSTOMER_API_KEY" \
     -H "Content-Type: application/json" -d '{}'
   ```

Requests under `/v1/api/` are checked against an allowlist. Global and admin
operations are blocked. Fleet and run plans are restricted to your configured providers. A plan
for a provider you did not configure is rejected.

Starting compute (`runs/apply`, `fleets/apply`) also needs an `Idempotency-Key` header, and it
needs credits. Without a verified price, the backend returns HTTP 402.

For a shell inside a run container, the customer registers a public key and asks for the
proxied command:

```sh
curl -X POST https://billing.example.com/v1/ssh-keys \
  -H "Authorization: Bearer $CUSTOMER_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"laptop\",\"key\":\"$(cat ~/.ssh/id_ed25519.pub)\"}"

curl https://billing.example.com/v1/runs/my-dev-env/ssh \
  -H "Authorization: Bearer $CUSTOMER_API_KEY"
```

The second call returns a command such as `ssh -p 2222 <job-id>@ssh.example.com`. That shell
runs inside the customer's own container. It does not reach the GPU host. The run must be
active and use a dev environment or another long-running configuration.

## 11. Billing is not active yet

Customer compute is blocked from billing until a verified USD-per-hour rate is configured and
matches the deployed dstack version. Usage intervals are recorded in the meantime. One shared
rate applies to all four providers, so per-provider price differences are not modeled. See the
"Billing limitation" section in the top-level `README.md`.

dstack's metrics documentation defines the instance price as USD per hour for the whole instance,
not per GPU. Confirm this on one live instance before you set the verified rate. Compare the
price dstack reports with the provider's console price for the same instance type.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Compose fails with a missing host path for `provider-backends.yml` | The file was not created in step 4 | Create `deploy/provider-backends.yml`, then start again. |
| Backend exits at startup, or the provider list is empty | The YAML has a typo, or a `type` is unknown or repeated | Fix the file. Unknown or duplicate `type` values stop startup. |
| `provider backend sync failed` in backend logs | dstack rejected a credential | Correct the credential and restart the backend. |
| `dstack admin endpoint must be an absolute HTTPS URL` | `DSTACK_URL` is not HTTPS | Set `DSTACK_URL=https://dstack-edge` in the backend environment. |
| Customer requests return 403 | Wrong API key, or the tenant is not provisioned yet | Check the key and wait for provisioning. |
| Customer requests return 402 | Insufficient credits, or the price is not verified | Expected until billing is verified. |
| `DSTACK_ENCRYPTION_KEY must contain exactly 32 bytes` | Key length is wrong | Regenerate with `openssl rand -hex 16`. |
| dstack admin token rejected after editing `.env` | dstack stores its token on first start | Keep `DSTACK_ADMIN_TOKEN` unchanged, or reset the dstack volume. Read `deploy/README.md` before resetting. |
| `GET /v1/runs/{name}/ssh` returns 409 | The run is not running yet, or dstack has no SSH proxy configured | Wait for the run, and check `DSTACK_SERVER_SSHPROXY_ADDRESS` and `DSTACK_SSHPROXY_API_TOKEN`. |
| SSH connection refused | The proxy port is not reachable from the customer | Check the TCP load balancer, `SSHPROXY_PUBLIC_ADDRESS`, and the loopback port mapping. |
| SSH returns `Permission denied (publickey)` | The key is not registered on the tenant account, or the run belongs to another tenant | List `/v1/ssh-keys`, re-register the public key, and check the run name. |
| SSH host key changed | The `sshproxy-keys` volume was recreated | Restore the volume from backup, or accept the new fingerprint and tell customers. |

## Backups

Dump both databases and back up the dstack server directory. The directory holds the AES key
that encrypts stored secrets. Follow the commands in `deploy/README.md`.

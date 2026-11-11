# 1engine

1engine is a unified platform for AI inference and GPU computing. It brings together GPU infrastructure management, AI workload deployment, model-serving endpoints, and scalable compute jobs behind one customer-facing service. The backend provides a common integration layer for supported infrastructure and runtime platforms, so customers can manage workloads across providers through consistent account, usage, and billing services.

The service manages customer accounts, provider integrations, GPU usage records, configurable pricing, prepaid credits, and payments. It uses Go 1.26, Gin, and PostgreSQL.

The current release includes dstack integration. Beta9 is a planned provider integration. GPU usage intervals are recorded, but customer charges stay disabled until the deployed dstack version's instance price is verified on a live instance.

## Run the service

1. Copy `.env.example` into the deployment secret store and replace every sample value. Use SMTP implicit TLS on port 465.
2. Configure PostgreSQL with TLS and a database role that can create the extension and schema objects in the migration.
3. Confirm that the installed dstack release supports `POST /api/users/refresh_token` and returns `creds.token` to an admin. Token refresh uses the dstack admin credential only on the dstack admin API. Customer requests use the returned tenant token.
4. Set `DSTACK_CUSTOMER_TOKEN_MODE=admin_refresh`. Provisioning fails closed if the installed dstack does not support this operation.
5. Run `go run ./cmd/server` from the repository root.

Set `PROVIDER_BACKENDS_FILE` to a read-only YAML file with one or more dstack backend
definitions for `aws`, `gcp`, `nebius`, or `runpod`. The backend applies them to tenant
projects and restricts customer fleet and run plans to those configured providers. Restart
the service after editing the file. Each instance is charged the USD-per-hour price that dstack
reports for that instance, as dstack does, so provider and instance-type prices are not shared.

To run the backend with a self-hosted dstack server and PostgreSQL in Docker, use [`deploy/README.md`](deploy/README.md).

The service applies pending migrations at startup. The backend is organized as a modular monolith around identity, tenancy, API keys, ledger, payments, compute, pricing, and dstack contexts. Each context owns its rules and persistence. HTTP handlers and server workers call context operations without issuing SQL. Cross-context operations pass the same transaction where one business action must update multiple contexts atomically. PostgreSQL adapters remain in the context packages, and `internal/store` owns connections, transaction query types, and migrations. The configured listener still needs a TLS-terminating reverse proxy.

## Bootstrap the provider account

Create and verify a portal account first. Then run the one-time command with the service configuration loaded, including `DATABASE_URL`.

```sh
go run ./cmd/provider-bootstrap provider-admin --email provider@example.com
```

The database permits one provider account. The command fails after the first successful bootstrap.

## Dstack servers

The backend can use one dstack server or several. Set `DSTACK_SERVERS` to a JSON list and the service provisions and places workloads on every server it names:

```sh
DSTACK_SERVERS='[
  {"name":"self-hosted","url":"https://dstack.internal","token":"admin-token","mode":"admin_refresh","provider_backends_file":"/config/provider-backends.yml"},
  {"name":"sky","url":"https://sky.dstack.ai","token":"user-token","mode":"shared_project"}
]'
```

Each entry has a unique `name`, an HTTPS `url`, a `token`, a `mode`, and an optional `provider_backends_file`. When `DSTACK_SERVERS` is set, `DSTACK_URL`, `DSTACK_ADMIN_TOKEN`, `DSTACK_CUSTOMER_TOKEN_MODE`, and `PROVIDER_BACKENDS_FILE` must be empty. Without `DSTACK_SERVERS`, those four settings define one server named `default`, so existing deployments need no change.

New tenants are created on the first server in the list and record it in `tenants.dstack_server`. Provisioning, proxying, and usage metering then use that server only, so a tenant draws compute from its own server's pools. Tenants created before this setting existed belong to `default`.

### Modes

`admin_refresh` is a server that grants global admin. The backend creates one dstack user and one project per tenant, adds the user to the project, writes the server's provider backends into that project, and mints the tenant token through `/api/users/refresh_token`.

`shared_project` is a server that gives only an ordinary account, as a hosted service does. The backend creates one project per tenant under that account, records the account in `tenants.dstack_account_username`, and stores the account token as the tenant credential. It creates no dstack users, adds no project members, writes no cloud backends, and never refreshes a tenant token. It first calls `/api/users/get_my_user` and fails closed unless the account may create projects.

Published plans on a `shared_project` server carry no `backends` selection, and a plan that sets one is rejected. SSH keys belong to the account on such a server, so `/api/users/public_keys/*` is blocked for tenants. Project isolation there rests on the proxy's project checks, not on dstack authorization, because every tenant shares one account token.

Charges stay blocked until the instance price is verified with `PUT /v1/provider/price`, whichever server supplied the instance.

## Test

```sh
go test ./...
go vet ./...
```

PostgreSQL integration tests run when `TEST_DATABASE_URL` points at an isolated test database. They migrate the database and create test records. Run all database-backed packages serially.

```sh
TEST_DATABASE_URL='postgres://billing_test:secret@localhost:5432/billing_test?sslmode=verify-full' go test -p 1 ./internal/apikey ./internal/compute ./internal/store
```

Install `go-mutesting` and `jq`, then run mutation tests for signup, login, rate limiting, and API-key behavior. Arithmetic mutations are disabled because this mutator emits invalid Go for those expressions in the module. The command fails when a mutant survives or cannot be executed. It writes `report.json` and `go-mutesting-report.html` in the repository root.

```sh
go install github.com/avito-tech/go-mutesting/cmd/go-mutesting@latest
TEST_DATABASE_URL='postgres://billing_test:secret@localhost:5432/billing_test?sslmode=verify-full' ./scripts/mutation-test.sh
```

## Billing limitation

The dstack observation worker records instance runtime intervals and charges each interval at the instance's reported price. The dstack metrics documentation defines `dstack_instance_price_dollars_per_hour` as the instance price in USD per hour, which is one price for the whole instance regardless of GPU count. The published instance schema does not state this. Keep customer charges blocked until one live instance confirms that its reported price matches the provider's console price for the same instance type. Then the provider marks the price verified with `PUT /v1/provider/price`, and charges start. Reservations at admission still use the verified rate from that endpoint.

## Main routes

- `POST /v1/signup`, `POST /v1/email-verifications/confirm`, and `POST /v1/login` manage portal identity.
- `GET /v1/me`, `/v1/me/credits`, `/v1/me/usage`, and `/v1/me/purchases` read the signed-in tenant's data.
- `POST /v1/api-keys`, `POST /v1/api-keys/{id}/rotate`, and `DELETE /v1/api-keys/{id}` manage customer keys.
- `POST /v1/credit-purchases` creates a fixed-package Snap checkout. Credits are granted only after a verified settlement.
- `/v1/dstack/*` and `/v1/api/*` proxy allowlisted dstack customer operations. Global and admin operations remain blocked.
- `/v1/provider/*` requires the provider account bootstrapped by the CLI.

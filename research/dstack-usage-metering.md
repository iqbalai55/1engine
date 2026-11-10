# dstack HTTP API as a source for commercial usage metering

**Research date:** 2026-11-08
**Scope:** Official dstack documentation, the official generated OpenAPI document, and first-party `dstackai/dstack` source only. This describes the currently published API, not a guarantee that every older self-hosted server exposes the same schema.

**Update 2026-11-10:** The dstack metrics page defines `dstack_instance_price_dollars_per_hour` as "Instance price, USD/hour", with the example `16.0` for an instance with `dstack_instance_gpu_count` of `4.0`. The release 0.20.28 offer list also prices whole instances (`B200:180GB:1` at $5.65, `B200:180GB:8` at $50.12). The unit is therefore per instance, not per GPU. This applies to the metrics and offer outputs. The instance schema in the OpenAPI document still does not state the unit, so the live check in `docs/dstack-tutorial.md` remains required before a verified rate is set. The "unit undocumented" statements below predate this update.

## Executive summary

dstack exposes useful data for constructing a commercial usage ledger: actual job/instance resource shape, GPU count and model, prices/rates, lifecycle states and timestamps, user/project attribution, and event history. The best source for realized compute is a combination of run/job data and instance data; job and instance metrics also expose runtime-seconds and USD/hour.

**The API is not documented as a billing/accounting system.** The published OpenAPI has no invoice, balance, payment, usage-accounting, or billing endpoints. It has a `Run.cost` number, but the schema does not document the field's accounting semantics, so it should not be equated with a customer charge without validating its meaning against the exact deployed dstack release. Offer and provisioning prices are useful rate metadata, not proof of a final bill. A commercial meter should persist observed lifecycle and rate snapshots itself, and reconcile them against the upstream provider or an agreed dstack pricing policy.

## What the API can support

### Actual allocation, duration, and price

- `POST /api/runs/list` and `POST /api/project/{project_name}/runs/get` return `Run` data; `Run.jobs[]` can include job specs and job submissions. The published `Run` schema has `cost`, but does not define its unit, whether it is estimated/settled, or how it is calculated. Treat it as an undocumented field for billing purposes. [HTTP API OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`Run-Output` schema; endpoints `runs/list`, `runs/get`).
- Job submission objects expose `submitted_at`, `finished_at` (nullable), `status`, termination fields, and `job_provisioning_data`. Provisioning data includes `price`, backend, instance type/resources, region, instance ID, and optional reservation. Runtime data can include the selected `offer`, whose instance resources and price are also available. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`JobSubmission-Output`, `JobProvisioningData`, `JobRuntimeData-Output`, `InstanceOfferWithAvailability`).
- `POST /api/instances/list` returns instances including ID/name/project/fleet, backend, instance type/resources, status, `created`, nullable `finished_at`, nullable `price`, region, health and termination information. `InstanceType.resources.gpus` is a list; count its elements for the GPU count represented by the instance type. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`Instance`, `InstanceType`, `Resources`, `Gpu`).
- The first-party Prometheus metrics explicitly define `dstack_instance_duration_seconds_total` as total instance runtime, `dstack_instance_price_dollars_per_hour` as instance USD/hour, and `dstack_instance_gpu_count`. Corresponding job metrics define `dstack_job_duration_seconds_total`, `dstack_job_price_dollars_per_hour` and `dstack_job_gpu_count`. These are strong signals for usage calculation, but the live metrics implementation selects only active instances and active jobs; it computes duration as `now - created_at` / `now - submitted_at`. Thus a scrape is a current-running snapshot/counter-like value, not a durable completed-usage ledger. [Metrics documentation](https://dstack.ai/docs/concepts/metrics/); [first-party implementation](https://github.com/dstackai/dstack/blob/master/src/dstack/_internal/server/services/prometheus/custom_metrics.py).
- A basic *inference* for gross resource cost is elapsed billable hours × applicable USD/hour × GPU count (if the rate is per GPU) or simply elapsed hours × instance price (if the rate is per instance). The dstack schemas do not define billing increments, rounding, minimum duration, interruptions, taxes, markup, or whether a quoted rate is GPU- or whole-instance-based. Do not assume that formula is an invoice calculation. Confirm rate units and policy before charging.

### Run and job lifecycle, status, and resource attribution

- Run statuses in the published schema are `pending`, `submitted`, `provisioning`, `running`, `terminating`, `terminated`, `failed`, and `done`. Job statuses include `submitted`, `provisioning`, `pulling`, `running`, `terminating`, `terminated`, `aborted`, `failed`, and `done`. A run may contain multiple jobs; jobs include replica/job numbers. Use job-level records for distributed runs/services rather than assuming one run equals one GPU allocation. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`RunStatus`, `JobStatus`, `Job-Output`, `JobSpec-Output`).
- Run-level timestamps include `submitted_at` and `last_processed_at`; the run schema has no `finished_at`. Each job submission has `submitted_at`, `last_processed_at` and nullable `finished_at`. Instance records have `created` and nullable `finished_at`. Lifecycle events can add timestamped transitions. This means a robust duration interval may need to use job/instance records and events—not just run fields. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`Run-Output`, `JobSubmission-Output`, `Instance`, `Event`).
- The run spec/configuration captures requested configuration and requirements; the job submission's provisioning data and runtime offer capture selected/realized instance information. Distinguish requested GPUs/resources from actual provisioned offer resources when metering. Actual job offer GPU data is nested under `job_runtime_data.offer.instance.resources.gpus`; provisioning data includes the instance type's resource list. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`RunSpec-Output`, `JobSpec-Output`, `JobProvisioningData`, `JobRuntimeData-Output`).
- `POST /api/project/{project_name}/runs/get` accepts JSON with `run_name` or `id`; `POST /api/runs/list` filters by `project_name`, `username`, `repo_id`, `only_active`, and supports pagination. List responses are sorted by descending `submitted_at`; continuation uses the previous item's `submitted_at` and `id`. `only_active: true` excludes finished and deleted runs. `include_jobs` defaults true, but job submissions returned per job are limited to at most 10 (latest only); older submissions are dropped when this limit is applied. Use direct run gets and page/reconcile records if complete submission history matters. [CLI & API guide](https://dstack.ai/docs/guides/cli-api/); [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`ListRunsRequest`; `runs/list` description).

### Offer and price metadata

- `POST /api/project/{project_name}/runs/get_plan` returns `RunPlan` with per-job `job_plans`; each plan can include offers, total offers and max price. The offer schema exposes backend, instance type/resources, region, numeric price, availability, availability zones, runtime type and block counts. A plan is a planning/offer snapshot, not evidence that the instance was provisioned at that price. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`RunPlan`, `JobPlan`, `InstanceOfferWithAvailability`).
- The CLI's official offer reference documents offer fields such as backend, region, instance type, resources, spot and price; its JSON sample has `offers[]` with `price`, `availability`, GPU name/vendor/memory and resources. This is helpful for interpreting metadata, but the docs describe the CLI output—not a separate offer-list HTTP endpoint. [dstack offer](https://dstack.ai/docs/reference/cli/dstack/offer/).
- The post-provisioning `JobProvisioningData.price` and instance `price` are more directly tied to an allocation than a pre-run plan offer. Still, only the Prometheus metric docs specify USD/hour explicitly. Preserve raw price, source field, unit assumption, backend, instance type, region, spot/on-demand attribute, and observation time. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json); [metrics documentation](https://dstack.ai/docs/concepts/metrics/).

### Events and audit trail

- `POST /api/events/list` returns events with UUID, message, `recorded_at`, actor (nullable for system actions), and resource targets. Targets can identify project, fleet, instance, run, job, volume, gateway, and other entity types. It supports filtering by target/within resource and pagination using `prev_recorded_at` and `prev_id`. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`Event`, `EventTarget`, `ListEventsRequest`; `events/list`).
- dstack describes events as chronological state-change/operation records, and its example shows run/job transitions, instance creation, job assignment/unassignment, and termination. They are useful for reconciling lifecycle boundaries and interpreting termination, but event messages are descriptive strings, not a structured billing record. [Events concept](https://dstack.ai/docs/concepts/events/).
- **Reliability limitation documented by dstack:** some events can become available through the API after their `recorded_at` timestamp. Pagination over recent events must account for this or delayed events may be missed. A practical *inference* is to re-read an overlapping recent time window and deduplicate by event ID, rather than advancing a one-way cursor at wall-clock time. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`events/list` description).
- Events are retained for 30 days by default; administrators can change `DSTACK_SERVER_EVENTS_TTL_SECONDS`, including setting it to `0` to disable storage. Therefore events are not a durable long-term ledger unless they are exported. [Events concept](https://dstack.ai/docs/concepts/events/); [environment variables](https://dstack.ai/docs/reference/env/).

### Instance data

`POST /api/instances/list` returns all instances visible to the user, sorted newest-first, with optional `project_names`, `fleet_ids`, `only_active`, and `include_imported` filters. Default page limit is 1,000; pagination is via `prev_created_at` + `prev_id`. Its `Instance` schema includes created/finished times, status (`pending`, `provisioning`, `idle`, `busy`, `terminating`, `terminated`), health/unreachable state, backend/type, fleet association, region/zone, price, and block counts. `InstanceType.resources.gpus` carries GPU model, memory and vendor. dstack's documented fleet-list endpoint includes only active fleet instances; use the instances list to retrieve all instances. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`instances/list`, `fleets/list`; `ListInstancesRequest`, `Instance`); [fleets API reference](https://dstack.ai/docs/reference/http/fleets/).

### Metrics API vs Prometheus

- The job metrics API is `GET /api/project/{project_name}/metrics/job/{run_name}` with optional `run_id`, `replica_num`, `job_num`, `limit`, `after`, `before`. It returns a `metrics` array; each metric has `name`, aligned `timestamps` and `values`. By default it returns one latest sample. Documented metrics are hardware-observation metrics (CPU/memory/GPU counts, utilization and memory), **not price or billed usage**. [HTTP API metrics reference](https://dstack.ai/docs/reference/http/metrics/); [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`JobMetrics`, `Metric`).
- Prometheus export is at `GET /metrics`, requires `DSTACK_ENABLE_PROMETHEUS_METRICS`, and includes instance/job runtime, price/hour, GPU counts and run outcome counters. The dstack documentation describes it as export/scrape, not a queryable historical billing store. Retention, scrape gaps, server restarts, and storage are the operator's responsibility; this last point is a practical inference from the scrape architecture, not an explicit billing guarantee. [Metrics concept](https://dstack.ai/docs/concepts/metrics/); [HTTP API reference](https://dstack.ai/docs/reference/http/metrics/).

## Billing/accounting API availability

The published OpenAPI currently lists server, users, authentication, projects, backends, fleets, runs, GPUs, metrics, logs, secrets, gateways, volumes, proxy, files, events, templates, and exports. It contains no billing, invoices, payments, account balance, or dedicated usage-accounting route. A search of the same published paths finds no `billing`, `account`, `invoice`, `payment`, or `usage` path. This is a statement about the published HTTP API, not an assertion that a hosted product's UI or a provider has no separate billing function. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json); [HTTP API index](https://dstack.ai/docs/reference/http/).

## Authentication and visibility

- The official API guide specifies `Authorization: Bearer <user token>`; requests in the guide use JSON with POST endpoints and `Content-Type: application/json`. The OpenAPI marks the main data endpoints (`runs/list`, `instances/list`, `events/list`, project metrics, and `/metrics`) with HTTP Bearer security. [CLI & API guide](https://dstack.ai/docs/guides/cli-api/); [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json).
- Visibility is token/user-scoped, not automatically organization-wide: list-runs and list-instances descriptions say “visible to user”; events are limited to the current user's own/project-member events, while global admins can see all. Project-scoped run-get/metrics endpoints require project membership in first-party router code. Do not use one ordinary customer user's token as a presumed cross-customer administrator feed. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (list endpoint descriptions); [first-party metrics router](https://github.com/dstackai/dstack/blob/master/src/dstack/_internal/server/routers/metrics.py).

## Polling and operational limits

1. Persist records locally: run and instance listings are paginated; events are paginated and can arrive late; run list submissions are capped at 10 per job; events expire after a configurable TTL. Use stable IDs and deduplicate updates/events. These limitations are documented in the OpenAPI descriptions and event/configuration docs above.
2. The API guide's example polls `/runs/get` every two seconds until a terminal status. This demonstrates a supported pattern, but the docs publish no global rate limit, webhook guarantee, maximum polling rate, or SLA for status freshness. The absence of a stated limit is not evidence of unlimited polling. [HTTP API usage example](https://dstack.ai/docs/reference/http/).
3. For ongoing runs, poll/list and capture state periodically; for final boundaries, reconcile run/job submission and instance `finished_at` plus lifecycle events. This is a recommended integration strategy (inference), not an official dstack billing recipe.
4. Capture server version at integration time using `POST /api/server/get_info`; the response schema includes nullable `server_version`. The OpenAPI `info.version` itself is `0.0.0`, so it does not identify a release. Test schema and semantics against the actual deployed server and record that `server_version` with each metering batch. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json) (`server/get_info`, `ServerInfo`).

## Documented request/response examples

The official guide demonstrates submission and polling in this form (both calls use `POST`, Bearer auth and JSON):

```http
POST /api/project/{project}/runs/apply
Authorization: Bearer <user token>
Content-Type: application/json

{"plan":{"run_spec":{"run_name":"hello-api","configuration":{"type":"task","commands":["echo hello world"]}}},"force":false}
```

```http
POST /api/project/{project}/runs/get
Authorization: Bearer <user token>
Content-Type: application/json

{"run_name":"hello-api"}
```

For list endpoints, the OpenAPI defines request/response models: for example, `runs/list` takes `ListRunsRequest` and returns an array of `Run`; `instances/list` takes `ListInstancesRequest` and returns an array of `Instance`; and `events/list` takes `ListEventsRequest` and returns an array of `Event`. The guide includes exact run submit/get examples; the model fields and pagination cursors above are from the generated OpenAPI. [Guide](https://dstack.ai/docs/guides/cli-api/); [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json).

## Version and source caveats

The official docs and OpenAPI are continuously published; their generated OpenAPI declares `info.version: "0.0.0"` and offers both `https://sky.dstack.ai` and `http://localhost:3000` as server URLs. The server can report its actual `server_version` via `/api/server/get_info`. First-party source links in this report point at the moving `master` branch and were checked on the research date; pin source to a release tag/commit when implementing against a production deployment. [OpenAPI](https://dstack.ai/docs/reference/http/openapi.json); [server API reference](https://dstack.ai/docs/reference/http/server/).

**Commercial metering conclusion:** use dstack as an operational evidence feed, not as a ledger of record for charges. Snapshot provisioned rate + actual resources and capture start/stop transitions durably, keep your own customer mapping/rate-card and billing rules, and reconcile discrepancies/late events. Treat price conversion, charge rounding, and `Run.cost` semantics as unresolved until confirmed for the exact deployment and commercial agreement.

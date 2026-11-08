CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE,
    password_hash text NOT NULL, email_verified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE provider_admins (
    slot smallint PRIMARY KEY DEFAULT 1 CHECK (slot = 1),
    user_id uuid NOT NULL UNIQUE REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE verification_challenges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id uuid NOT NULL UNIQUE REFERENCES users(id),
    token_hash bytea NOT NULL UNIQUE, expires_at timestamptz NOT NULL, consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE portal_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), user_id uuid NOT NULL REFERENCES users(id),
    token_hash bytea NOT NULL UNIQUE, expires_at timestamptz NOT NULL, revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE tenants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner_user_id uuid NOT NULL UNIQUE REFERENCES users(id),
    dstack_username text NOT NULL UNIQUE, dstack_project text NOT NULL UNIQUE,
    dstack_provision_state text NOT NULL DEFAULT 'pending' CHECK (dstack_provision_state IN ('pending','ready','failed')),
    state text NOT NULL DEFAULT 'provisioning' CHECK (state IN ('provisioning','ready','suspended')),
    markup_bps integer NOT NULL DEFAULT 1000 CHECK (markup_bps BETWEEN 0 AND 10000), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE tenant_dstack_credentials (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id), ciphertext bytea NOT NULL, key_version text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE api_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES tenants(id), name text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE, created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
);
CREATE INDEX api_keys_active_tenant_idx ON api_keys(tenant_id) WHERE revoked_at IS NULL;
CREATE TABLE credit_accounts (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id), balance_micros bigint NOT NULL DEFAULT 0,
    version bigint NOT NULL DEFAULT 0, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE credit_entries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES credit_accounts(tenant_id),
    delta_micros bigint NOT NULL CHECK (delta_micros <> 0), kind text NOT NULL CHECK (kind IN ('purchase','usage','refund','reversal')),
    idempotency_key text NOT NULL UNIQUE, source text NOT NULL, snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(tenant_id,idempotency_key)
);
CREATE INDEX credit_entries_tenant_created_idx ON credit_entries(tenant_id,created_at DESC);
CREATE TABLE purchases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES tenants(id),
    package_size integer NOT NULL CHECK (package_size IN (10,50,100)), credit_micros bigint NOT NULL CHECK (credit_micros > 0),
    amount_idr bigint NOT NULL CHECK (amount_idr IN (180000,900000,1800000)), order_id text NOT NULL UNIQUE,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','settlement','deny','cancel','expire','refund')),
    refunded_idr bigint NOT NULL DEFAULT 0 CHECK (refunded_idr >= 0), snap_redirect_url text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX purchases_open_idx ON purchases(created_at) WHERE state='pending';
CREATE TABLE payment_observations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), purchase_id uuid NOT NULL REFERENCES purchases(id),
    source text NOT NULL CHECK (source IN ('notification','status')), event_key text NOT NULL, status text NOT NULL,
    gross_amount text NOT NULL, payload jsonb NOT NULL, observed_at timestamptz NOT NULL DEFAULT now(), UNIQUE(purchase_id,source,event_key)
);
CREATE TABLE usage_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id uuid NOT NULL REFERENCES tenants(id), allocation_id text NOT NULL,
    submission_id text NOT NULL DEFAULT '', interval_start timestamptz NOT NULL, interval_end timestamptz NOT NULL,
    gpu_count integer NOT NULL CHECK (gpu_count >= 0), rate_usd_nano bigint,
    rate_unit text NOT NULL CHECK (rate_unit IN ('instance_hour','gpu_hour','unpriced')), usd_nano bigint,
    markup_bps integer NOT NULL CHECK (markup_bps BETWEEN 0 AND 10000), credit_delta_micros bigint NOT NULL CHECK (credit_delta_micros >= 0),
    source_version text NOT NULL, provenance jsonb NOT NULL DEFAULT '{}'::jsonb, created_at timestamptz NOT NULL DEFAULT now(),
    CHECK(interval_end > interval_start), UNIQUE(tenant_id,allocation_id,interval_start,interval_end)
);
CREATE TABLE dstack_events (
    tenant_id uuid NOT NULL REFERENCES tenants(id), event_id text NOT NULL, observed_at timestamptz NOT NULL,
    payload jsonb NOT NULL, PRIMARY KEY(tenant_id,event_id)
);
CREATE TABLE usage_cursors (
    tenant_id uuid PRIMARY KEY REFERENCES tenants(id), cursor timestamptz NOT NULL DEFAULT 'epoch', updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE dstack_runs (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    run_id text NOT NULL,
    payload jsonb NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(tenant_id,run_id)
);
CREATE TABLE dstack_instance_prices (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(), price_key text NOT NULL UNIQUE,
    hourly_usd_nano bigint NOT NULL CHECK(hourly_usd_nano > 0), rate_unit text NOT NULL CHECK(rate_unit='instance_hour'),
    verified boolean NOT NULL DEFAULT false, version text NOT NULL, verified_by uuid REFERENCES users(id), observed_at timestamptz NOT NULL DEFAULT now()
);

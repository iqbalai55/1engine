CREATE TABLE compute_reservations (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    request_key text NOT NULL,
    request_hash bytea NOT NULL,
    reserved_micros bigint NOT NULL CHECK (reserved_micros > 0),
    state text NOT NULL CHECK (state IN ('pending','reserved','released')),
    expires_at timestamptz NOT NULL,
    response_status integer,
    response_body bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, request_key),
    CHECK ((response_status IS NULL) = (response_body IS NULL)),
    CHECK (state <> 'released' OR response_status IS NULL)
);
CREATE INDEX compute_reservations_available_idx ON compute_reservations(tenant_id,expires_at) WHERE state IN ('pending','reserved');

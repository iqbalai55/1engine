ALTER TABLE tenants ADD COLUMN dstack_project_id uuid;

CREATE TABLE dstack_observations (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    stream text NOT NULL CHECK (stream IN ('runs','instances','events')),
    observation_id text NOT NULL,
    source_version text NOT NULL,
    observed_at timestamptz NOT NULL,
    payload jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id,stream,observation_id,source_version)
);
CREATE INDEX dstack_observations_recent_idx ON dstack_observations(tenant_id,stream,observed_at DESC);

CREATE TABLE dstack_poll_cursors (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    stream text NOT NULL CHECK (stream IN ('runs','instances','events')),
    checkpoint_at timestamptz,
    checkpoint_id text,
    resume_at timestamptz,
    resume_id text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id,stream),
    CHECK ((resume_at IS NULL) = (resume_id IS NULL))
);

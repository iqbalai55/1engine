#!/bin/sh
set -eu

: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${BILLING_DB_ROLE:?BILLING_DB_ROLE is required}"
: "${BILLING_DB_PASSWORD:?BILLING_DB_PASSWORD is required}"
: "${BILLING_DB_NAME:?BILLING_DB_NAME is required}"
: "${DSTACK_DB_ROLE:?DSTACK_DB_ROLE is required}"
: "${DSTACK_DB_PASSWORD:?DSTACK_DB_PASSWORD is required}"
: "${DSTACK_DB_NAME:?DSTACK_DB_NAME is required}"

PGHOST= PGHOSTADDR= psql \
    --username "$POSTGRES_USER" \
    --dbname postgres \
    --no-password \
    --no-psqlrc \
    --set=ON_ERROR_STOP=1 \
    --set=billing_role="$BILLING_DB_ROLE" \
    --set=billing_password="$BILLING_DB_PASSWORD" \
    --set=billing_db="$BILLING_DB_NAME" \
    --set=dstack_role="$DSTACK_DB_ROLE" \
    --set=dstack_password="$DSTACK_DB_PASSWORD" \
    --set=dstack_db="$DSTACK_DB_NAME" <<'EOSQL'
SELECT format('CREATE ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD %L', :'billing_role', :'billing_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'billing_role')
\gexec

SELECT format('CREATE ROLE %I WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD %L', :'dstack_role', :'dstack_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'dstack_role')
\gexec

ALTER ROLE :"billing_role" WITH LOGIN PASSWORD :'billing_password';
ALTER ROLE :"dstack_role" WITH LOGIN PASSWORD :'dstack_password';

SELECT format('CREATE DATABASE %I WITH OWNER %I ENCODING %L', :'billing_db', :'billing_role', 'UTF8')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'billing_db')
\gexec

SELECT format('CREATE DATABASE %I WITH OWNER %I ENCODING %L', :'dstack_db', :'dstack_role', 'UTF8')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'dstack_db')
\gexec
EOSQL

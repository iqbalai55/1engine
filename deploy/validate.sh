#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

ENV_FILE=.env.example
COMPOSE_FILE=compose.yaml
suffix="$$-$(date +%s)"
BACKEND_IMAGE="1inference/backend:validate-$suffix"
CERT_INIT_IMAGE="1inference/cert-init:validate-$suffix"
COMPOSE=(env "BACKEND_IMAGE=$BACKEND_IMAGE" "CERT_INIT_IMAGE=$CERT_INIT_IMAGE" docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" --profile bootstrap)

required_vars=(
    POSTGRES_SUPERUSER_PASSWORD
    BILLING_DB_PASSWORD
    DSTACK_DB_PASSWORD
    DSTACK_ADMIN_TOKEN
    SESSION_SECRET
    DSTACK_ENCRYPTION_KEY
    MIDTRANS_SERVER_KEY
    MIDTRANS_CLIENT_KEY
    PUBLIC_BASE_URL
    GOOGLE_CLIENT_ID
    GOOGLE_CLIENT_SECRET
    GOOGLE_REDIRECT_URL
    PROVIDER_ADMIN_EMAIL
)

for file in README.md decision.md compose.yaml Caddyfile .env.example certs/Dockerfile certs/generate.sh postgres/init-databases.sh; do
    [ -f "$file" ] || {
        printf 'FAIL deployment file is missing: %s\n' "$file" >&2
        exit 1
    }
done

fail() {
    printf 'FAIL %s\n' "$1" >&2
    exit 1
}

pass() {
    printf 'ok   %s\n' "$1"
}

rendered=""
ca_volume=""
trust_volume=""
dstack_cert_volume=""
postgres_cert_volume=""
dstack_data_volume=""

cleanup() {
    if [ -n "$rendered" ]; then
        rm -f "$rendered"
    fi
    docker volume rm -f "$ca_volume" "$trust_volume" "$dstack_cert_volume" \
        "$postgres_cert_volume" "$dstack_data_volume" >/dev/null 2>&1 || true
    docker image rm -f "$BACKEND_IMAGE" "$CERT_INIT_IMAGE" >/dev/null 2>&1 || true
    return 0
}

trap cleanup EXIT

command -v docker >/dev/null 2>&1 || fail "docker is not installed"
docker compose version >/dev/null 2>&1 || fail "docker compose is not available"
command -v python3 >/dev/null 2>&1 || fail "python3 is not installed"

[ -f "$ENV_FILE" ] || fail "$ENV_FILE is missing"

for name in "${required_vars[@]}"; do
    value=$(sed -n "s/^${name}=//p" "$ENV_FILE" | head -n 1)
    [ -n "$value" ] || fail "$ENV_FILE does not set $name"
done
pass "$ENV_FILE sets every required variable"
pass "deployment instructions, decision record, and runtime files are present"

"${COMPOSE[@]}" config --quiet >/dev/null || fail "compose config failed"
rendered=$(mktemp) || fail "could not create a temporary file"
"${COMPOSE[@]}" config --format json >"$rendered" || fail "compose config failed to render"
pass "compose config renders"

python3 - "$rendered" <<'PYTHON' || fail "the rendered compose model breaks a deployment rule"
import json
import sys

model = json.load(open(sys.argv[1]))
services = model["services"]

problems = []


def check(condition, message):
    if not condition:
        problems.append(message)


for name, service in services.items():
    for port in service.get("ports", []):
        check(
            port.get("host_ip") == "127.0.0.1",
            f"{name} publishes port {port.get('published')} on {port.get('host_ip')}",
        )

for name in ("dstack", "postgres"):
    check(not services[name].get("ports"), f"{name} publishes a host port")

volumes = {
    name: {mount.get("source"): mount for mount in service.get("volumes", [])}
    for name, service in services.items()
}


def mounted_by(volume):
    return sorted(name for name, mounts in volumes.items() if volume in mounts)


check(mounted_by("ca-keys") == ["cert-init"], "the CA private key volume is mounted outside cert-init")
check(
    mounted_by("trust") == ["backend", "bootstrap", "cert-init", "dstack"],
    "the trust volume is mounted by an unexpected service",
)
check(mounted_by("dstack-certs") == ["caddy", "cert-init"], "the dstack certificate volume is mounted by an unexpected service")
check(mounted_by("postgres-certs") == ["cert-init", "postgres"], "the postgres certificate volume is mounted by an unexpected service")

for name in ("backend", "bootstrap", "dstack"):
    for source, mount in volumes[name].items():
        if source == "trust":
            check(mount.get("read_only") is True, f"{name} mounts the trust volume writable")

check(
    services["dstack"]["environment"].get("PGSSLROOTCERT") == "/trust/ca.crt",
    "dstack does not point PGSSLROOTCERT at the private CA",
)
check(
    services["backend"]["environment"].get("SSL_CERT_FILE") == "/trust/ca-bundle.pem",
    "the backend does not point SSL_CERT_FILE at the trust bundle",
)
check(
    "ssl=verify-full" in services["dstack"]["environment"].get("DSTACK_DATABASE_URL", ""),
    "the dstack database URL does not require certificate verification",
)
check(
    "sslmode=verify-full" in services["backend"]["environment"].get("DATABASE_URL", ""),
    "the backend database URL does not require certificate verification",
)
check(
    services["dstack"]["environment"].get("DSTACK_SERVER_ADMIN_TOKEN")
    == services["backend"]["environment"].get("DSTACK_ADMIN_TOKEN"),
    "the dstack server and the backend do not share one admin token",
)
check(
    services["backend"]["environment"].get("DSTACK_URL") == "https://dstack-edge",
    "the backend does not reach dstack through the TLS endpoint",
)

if problems:
    for problem in problems:
        print(f"problem: {problem}", file=sys.stderr)
    sys.exit(1)
PYTHON
pass "the rendered compose model keeps every port on loopback and every secret inside a volume"

"${COMPOSE[@]}" build backend cert-init >/dev/null || fail "the image build failed"
pass "the backend and cert-init images build"

ca_volume="1inference-validate-ca-${suffix}"
trust_volume="1inference-validate-trust-${suffix}"
dstack_cert_volume="1inference-validate-dstack-certs-${suffix}"
postgres_cert_volume="1inference-validate-postgres-certs-${suffix}"
dstack_data_volume="1inference-validate-dstack-data-${suffix}"

docker run --rm \
    -v "$ca_volume:/ca" \
    -v "$trust_volume:/trust" \
    -v "$dstack_cert_volume:/out/dstack" \
    -v "$postgres_cert_volume:/out/postgres" \
    -v "$dstack_data_volume:/dstack-server" \
    "$CERT_INIT_IMAGE" >/dev/null 2>&1 || fail "the certificate image failed"

for path in /ca/ca.key /ca/ca.crt /trust/ca.crt /trust/ca-bundle.pem \
    /out/dstack/tls.crt /out/dstack/tls.key \
    /out/postgres/tls.crt /out/postgres/tls.key /dstack-server/config.yml; do
    docker run --rm --entrypoint sh \
        -v "$ca_volume:/ca:ro" \
        -v "$trust_volume:/trust:ro" \
        -v "$dstack_cert_volume:/out/dstack:ro" \
        -v "$postgres_cert_volume:/out/postgres:ro" \
        -v "$dstack_data_volume:/dstack-server:ro" \
        "$CERT_INIT_IMAGE" \
        -c 'test -s "$1"' sh "$path" || fail "the certificate image did not create $path"
done

owner=$(docker run --rm --entrypoint stat \
    -v "$postgres_cert_volume:/out/postgres:ro" \
    "$CERT_INIT_IMAGE" \
    -c '%u:%g' /out/postgres/tls.key)
[ "$owner" = "0:0" ] || fail "the postgres key is owned by $owner instead of root"
mode=$(docker run --rm --entrypoint stat \
    -v "$postgres_cert_volume:/out/postgres:ro" \
    "$CERT_INIT_IMAGE" \
    -c '%a' /out/postgres/tls.key)
[ "$mode" = "600" ] || fail "the postgres key mode is $mode instead of 600"

config=$(docker run --rm --entrypoint cat \
    -v "$dstack_data_volume:/dstack-server:ro" \
    "$CERT_INIT_IMAGE" \
    /dstack-server/config.yml)
case "$config" in
    *"projects: []"*) ;;
    *) fail "the dstack server config does not declare an empty project list" ;;
esac
case "$config" in
    *"allow_non_admins_create_projects: false"*) ;;
    *) fail "the dstack server config still allows non-admin project creation" ;;
esac
case "$config" in
    *"allow_non_admins_manage_ssh_fleets: false"*) ;;
    *) fail "the dstack server config still allows SSH fleet management" ;;
esac

for leaf in /out/dstack/tls.crt /out/postgres/tls.crt; do
    case "$leaf" in
        /out/dstack/*) volume="$dstack_cert_volume" ;;
        *) volume="$postgres_cert_volume" ;;
    esac
    docker run --rm --entrypoint openssl \
        -v "$ca_volume:/ca:ro" \
        -v "$volume:/out/leaf:ro" \
        "$CERT_INIT_IMAGE" \
        verify -CAfile /ca/ca.crt /out/leaf/tls.crt >/dev/null 2>&1 \
        || fail "the certificate $leaf does not chain to the CA"
done

bundle_size=$(docker run --rm --entrypoint sh \
    -v "$trust_volume:/trust:ro" \
    "$CERT_INIT_IMAGE" \
    -c 'wc -c < /trust/ca-bundle.pem')
ca_size=$(docker run --rm --entrypoint sh \
    -v "$ca_volume:/ca:ro" \
    "$CERT_INIT_IMAGE" \
    -c 'wc -c < /ca/ca.crt')
[ "$bundle_size" -gt "$ca_size" ] || fail "the trust bundle does not carry the public roots"

pass "the certificate image creates a CA, two signed certificates, and the trust bundle"

printf '\nAll deployment checks passed.\n'

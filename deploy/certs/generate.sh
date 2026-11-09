#!/bin/sh
set -eu

CA_DIR=${CA_DIR:-/ca}
TRUST_DIR=${TRUST_DIR:-/trust}
DSTACK_CERT_DIR=${DSTACK_CERT_DIR:-/out/dstack}
POSTGRES_CERT_DIR=${POSTGRES_CERT_DIR:-/out/postgres}
DSTACK_SERVER_DIR=${DSTACK_SERVER_DIR:-/dstack-server}

CA_KEY="$CA_DIR/ca.key"
CA_CRT="$CA_DIR/ca.crt"
DSTACK_CONFIG="$DSTACK_SERVER_DIR/config.yml"
PUBLIC_ROOTS=/etc/ssl/certs/ca-certificates.crt

mkdir -p "$CA_DIR" "$TRUST_DIR" "$DSTACK_CERT_DIR" "$POSTGRES_CERT_DIR" "$DSTACK_SERVER_DIR"

if [ ! -s "$CA_KEY" ] || [ ! -s "$CA_CRT" ]; then
    openssl req -x509 -newkey rsa:4096 -nodes -sha256 -days 3650 \
        -keyout "$CA_KEY" \
        -out "$CA_CRT" \
        -subj "/CN=1Inference Internal CA/O=1Inference" \
        -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
        -addext "keyUsage=critical,keyCertSign,cRLSign"
    chmod 0600 "$CA_KEY"
    chmod 0644 "$CA_CRT"
fi

issue_leaf() {
    cn=$1
    san=$2
    out_dir=$3
    key="$out_dir/tls.key"
    crt="$out_dir/tls.crt"
    ext_file="$out_dir/openssl.ext"

    if [ -s "$key" ] && [ -s "$crt" ]; then
        return 0
    fi

    cat > "$ext_file" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$san
EOF

    openssl req -new -newkey rsa:2048 -nodes -sha256 \
        -keyout "$key" \
        -out "$out_dir/tls.csr" \
        -subj "/CN=$cn/O=1Inference"
    serial=$(openssl rand -hex 16)
    openssl x509 -req -in "$out_dir/tls.csr" -sha256 -days 825 \
        -CA "$CA_CRT" -CAkey "$CA_KEY" -set_serial "0x$serial" \
        -extfile "$ext_file" \
        -out "$crt"
    rm -f "$out_dir/tls.csr" "$ext_file"

    chmod 0600 "$key"
    chmod 0644 "$crt"
}

issue_leaf "dstack-edge" "DNS:dstack-edge,DNS:localhost,IP:127.0.0.1" "$DSTACK_CERT_DIR"
issue_leaf "postgres" "DNS:postgres" "$POSTGRES_CERT_DIR"

chmod 0600 "$POSTGRES_CERT_DIR/tls.key"

cp "$CA_CRT" "$TRUST_DIR/ca.crt"
cat "$PUBLIC_ROOTS" "$CA_CRT" > "$TRUST_DIR/ca-bundle.pem"
chmod 0644 "$TRUST_DIR/ca.crt" "$TRUST_DIR/ca-bundle.pem"

if [ ! -s "$DSTACK_CONFIG" ]; then
    encryption_key=$(openssl rand -base64 32)
    cat > "$DSTACK_CONFIG" <<EOF
projects: []
encryption:
  keys:
    - type: aes
      name: key1
      secret: "$encryption_key"
default_permissions:
  allow_non_admins_create_projects: false
  allow_non_admins_manage_ssh_fleets: false
EOF
    chmod 0600 "$DSTACK_CONFIG"
fi

echo "certificate material is present"

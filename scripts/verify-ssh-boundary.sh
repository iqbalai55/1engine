#!/bin/sh
set -eu

FUZZTIME=${FUZZTIME:-30s}
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$ROOT"

run_go() {
	go test ./internal/dstack -run '^$' -fuzz '^FuzzRunPlanStripsCustomerSSHKey$' -fuzztime "$FUZZTIME"
	go test ./internal/dstack -run '^$' -fuzz '^FuzzResponseScrubsDirectHostAccess$' -fuzztime "$FUZZTIME"
}

if command -v go >/dev/null 2>&1; then
	run_go
	exit 0
fi

docker run --rm -v "$ROOT":/src -w /src -e GOFLAGS=-mod=mod golang:1.26.11-alpine sh -c "
	go test ./internal/dstack -run '^\$' -fuzz '^FuzzRunPlanStripsCustomerSSHKey\$' -fuzztime $FUZZTIME &&
	go test ./internal/dstack -run '^\$' -fuzz '^FuzzResponseScrubsDirectHostAccess\$' -fuzztime $FUZZTIME
"

#!/bin/sh
set -eu

if [ -z "${TEST_DATABASE_URL:-}" ]; then
	printf '%s\n' 'TEST_DATABASE_URL must point to an isolated PostgreSQL test database' >&2
	exit 1
fi

mutator=$(command -v go-mutesting || true)
if [ -z "$mutator" ]; then
	mutator="$(go env GOPATH)/bin/go-mutesting"
fi
if ! command -v jq >/dev/null 2>&1; then
	printf '%s\n' 'jq is required to validate the mutation report' >&2
	exit 1
fi
if [ ! -x "$mutator" ]; then
	printf '%s\n' 'Install go-mutesting with: go install github.com/avito-tech/go-mutesting/cmd/go-mutesting@latest' >&2
	exit 1
fi

export GOTOOLCHAIN=local
set +e
"$mutator" \
	--exec-timeout=30 \
	--html-output \
	--disable='arithmetic/*' \
	--disable='numbers/*' \
	--exec="$(dirname "$0")/run-mutated-go-tests.sh" \
	--match='^(Allow|rateLimit|googleStart|googleCallback|create|Issue|Rotate|Revoke|List|Authenticate)$' \
	./internal/httpapi \
	./internal/apikey
status=$?
set -e
if [ "$status" -ne 0 ]; then
	exit "$status"
fi

skipped=$(jq -r '.stats.skippedCount' report.json)
executed=$(jq -r '.stats.killedCount + .stats.escapedCount' report.json)
killed=$(jq -r '.stats.killedCount' report.json)
escaped=$(jq -r '.stats.escapedCount' report.json)
if [ "$executed" -eq 0 ]; then
	printf '%s\n' 'mutation run executed no mutants. See report.json for details.' >&2
	exit 1
fi
printf 'mutation score excluding skipped mutants: %s/%s killed (%s survived, %s skipped)\n' \
	"$killed" "$executed" "$escaped" "$skipped"
if [ "$skipped" -ne 0 ]; then
	printf 'mutation run skipped %s mutants and is incomplete. See report.json for details.\n' "$skipped" >&2
	exit 1
fi
if [ "$escaped" -ne 0 ]; then
	printf 'mutation run left %s mutants alive. See report.json for details.\n' "$escaped" >&2
	exit 1
fi

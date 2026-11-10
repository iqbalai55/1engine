#!/bin/sh
set -eu

if [ -z "${MUTATE_CHANGED:-}" ] || [ -z "${MUTATE_ORIGINAL:-}" ] || [ -z "${MUTATE_PACKAGE:-}" ]; then
	printf '%s\n' 'go-mutesting did not provide the mutation paths' >&2
	exit 2
fi

original=$(realpath -m "$MUTATE_ORIGINAL")
changed=$(realpath -m "$MUTATE_CHANGED")
package=$(realpath "$MUTATE_PACKAGE")
if [ ! -f "$original" ]; then
	if [ -f "$changed" ]; then
		mutant_dir=$(dirname "$changed")
		base_name=$(basename "$changed")
		candidate="$mutant_dir/${base_name%.*}.original"
		if [ -f "$candidate" ]; then
			original=$(realpath -m "$candidate")
		fi
	fi
fi
backup_dir="${XDG_CACHE_HOME:-${HOME:-/tmp}/.cache}/go-mutesting/backups"
mkdir -p "$backup_dir"
backup="$backup_dir/$(printf '%s' "$original" | sha256sum | cut -c1-32)"

if [ -e "$backup" ]; then
	mv "$backup" "$original"
fi
if [ ! -f "$original" ]; then
	printf '%s\n' "original source does not exist at $original" >&2
	exit 2
fi
if [ ! -f "$changed" ]; then
	printf '%s\n' "mutated source does not exist at $changed" >&2
	mutant_dir=$(dirname "$changed")
	base_name=$(basename "$changed")
	changed="$mutant_dir/${base_name%.*}"
	if [ ! -f "$changed" ]; then
		printf '%s\n' "mutated source does not exist at $changed" >&2
		exit 2
	fi
fi

mutation_diff=$(diff -u "$original" "$changed" || true)
test_output=$(mktemp)
mv "$original" "$backup"
restore() {
	if [ -e "$backup" ]; then
		mv "$backup" "$original"
	fi
	rm -f "$test_output"
}
trap restore EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
cp "$changed" "$original"

cd "$package"
if GOTOOLCHAIN=local go test -count=1 -timeout "${MUTATE_TIMEOUT}s" . >"$test_output" 2>&1; then
	cat "$test_output"
	printf '%s\n' "$mutation_diff"
	exit 1
else
	exit_code=$?
	cat "$test_output"
	if [ "$exit_code" -eq 1 ] && grep -qE '^FAIL[[:space:]].*\[build failed\]$' "$test_output"; then
		exit 0
	fi
	if grep -Eq '(panic: test timed out|no Go files in|cannot find package)' "$test_output"; then
		exit 2
	fi
	exit 0
fi

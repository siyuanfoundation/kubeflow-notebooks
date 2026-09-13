#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 2 ]]; then
  printf 'Usage: bash gke/scripts/plan.sh CONFIG_JSON NEW_OUTPUT_DIRECTORY\n' >&2
  exit 2
fi

root=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)
config=$(realpath "$1")
output=$(realpath -m "$2")
if [[ -e "$output" ]]; then
  printf 'Output already exists; choose a new directory to preserve the prior plan.\n' >&2
  exit 2
fi
command -v go >/dev/null
command -v kubectl >/dev/null
mkdir -p "$(dirname "$output")"
temporary=$(mktemp -d "$(dirname "$output")/.notebooks-plan.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
cd "$root/gke"
go build -o "$temporary/render" ./cmd/render
for stage in namespaces isolation applications edge; do
  "$temporary/render" --repo-root "$root" --config "$config" --stage "$stage" > "$temporary/$stage.json"
done
rm "$temporary/render"
git -C "$root" rev-parse HEAD > "$temporary/upstream-revision.txt"
sha256sum "$config" > "$temporary/config.sha256"
mv "$temporary" "$output"
trap - EXIT
printf 'Rendered plan to %s. No cluster or cloud resources were changed.\n' "$output"

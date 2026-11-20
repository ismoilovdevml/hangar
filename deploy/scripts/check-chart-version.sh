#!/usr/bin/env bash
# Checks that the Helm chart's version and appVersion follow the release, so
# a default `helm install` pulls the image of this release (and the matching
# hangar-scanner image) rather than an old one.
#
#   deploy/scripts/check-chart-version.sh v0.1.9   # the release workflow: must equal the tag
#   deploy/scripts/check-chart-version.sh          # locally (make chart-version-check)
#
# With a tag, version and appVersion must both equal it (without the "v").
# Without one, the tag on HEAD is used when there is one; otherwise both must
# be equal and not older than the newest v* tag reachable from HEAD.

set -euo pipefail

chart=$(cd "$(dirname "${BASH_SOURCE[0]}")/../helm/hangar" && pwd)/Chart.yaml

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# field NAME: the top-level NAME of Chart.yaml without quotes.
field() {
	awk -v k="$1" '$1 == k ":" { v = $2; gsub(/["'\'']/, "", v); print v; exit }' "$chart"
}

version=$(field version)
app=$(field appVersion)
[[ -n "$version" && -n "$app" ]] || die "$chart has no version or appVersion"
[[ "$version" == "$app" ]] ||
	die "$chart: version $version and appVersion $app differ; set both to the release version"

tag=${1:-}
if [[ -z "$tag" ]]; then
	tag=$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null || true)
fi
if [[ -n "$tag" ]]; then
	want=${tag#v}
	[[ "$app" == "$want" ]] ||
		die "$chart: appVersion $app does not match the release $tag; set version and appVersion to \"$want\""
	echo "chart version: $app matches $tag"
	exit 0
fi

latest=$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null || true)
if [[ -z "$latest" ]]; then
	echo "chart version: $app (no release tag reachable from HEAD to compare with)"
	exit 0
fi
latest=${latest#v}
oldest=$(printf '%s\n%s\n' "$app" "$latest" | sort -V | head -n 1)
if [[ "$app" != "$latest" && "$oldest" == "$app" ]]; then
	die "$chart: appVersion $app is older than the latest release v$latest; set version and appVersion to the next release"
fi
echo "chart version: $app (latest release v$latest)"

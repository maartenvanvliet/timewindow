#!/usr/bin/env bash
#
# Print the next release tag, from the highest existing one and a bump level.
#
#   script/next-version.sh patch   # v1.2.3 -> v1.2.4
#   script/next-version.sh minor   # v1.2.3 -> v1.3.0
#   script/next-version.sh major   # v1.2.3 -> v2.0.0
#
# Only vMAJOR.MINOR.PATCH tags count, so a pre-release (v1.3.0-rc.1) never
# becomes the base for the next version. With no tags at all it starts at
# v0.1.0.

set -euo pipefail

bump=${1:-patch}

latest=$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)

if [ -z "$latest" ]; then
	echo "v0.1.0"
	exit 0
fi

IFS=. read -r major minor patch <<<"${latest#v}"

case "$bump" in
major)
	major=$((major + 1))
	minor=0
	patch=0
	;;
minor)
	minor=$((minor + 1))
	patch=0
	;;
patch)
	patch=$((patch + 1))
	;;
*)
	echo "unknown bump level '$bump': want major, minor or patch" >&2
	exit 1
	;;
esac

printf 'v%d.%d.%d\n' "$major" "$minor" "$patch"

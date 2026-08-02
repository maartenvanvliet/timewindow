#!/usr/bin/env bash
#
# Build the release archives into dist/.
#
# This is what the release workflow runs, so a release can be reproduced
# locally with the same command:
#
#   VERSION=v1.2.3 ./script/build-release.sh
#
# With no VERSION it uses `git describe`, so local builds are clearly marked
# as untagged or dirty.

set -euo pipefail

BINARY=timewindow
DIST=${DIST:-dist}

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}
# Honour SOURCE_DATE_EPOCH so the same source can produce the same archives.
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
	DATE=${DATE:-$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)}
else
	DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
fi

# GOOS/GOARCH pairs to ship.
TARGETS=${TARGETS:-"
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
"}

rm -rf "$DIST"
mkdir -p "$DIST"

echo "building $BINARY $VERSION ($COMMIT, $DATE)"

for target in $TARGETS; do
	goos=${target%/*}
	goarch=${target#*/}

	name="${BINARY}_${VERSION}_${goos}_${goarch}"
	stage="$DIST/$name"
	mkdir -p "$stage"

	exe=$BINARY
	if [ "$goos" = windows ]; then
		exe="$BINARY.exe"
	fi
	binary="$stage/$exe"

	# CGO off keeps the binaries static; -trimpath keeps build paths out of
	# them so the same source builds byte-for-byte on another machine.
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build \
		-trimpath \
		-ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
		-o "$binary" \
		.

	# Archive members sit at the root, with no wrapping directory, so a
	# caller can pull out just the binary:
	#   curl -sSfL <url> | tar xz timewindow
	contents=("$exe")
	for extra in README.md LICENSE; do
		if [ -f "$extra" ]; then
			cp "$extra" "$stage/"
			contents+=("$extra")
		fi
	done

	if [ "$goos" = windows ]; then
		(cd "$stage" && zip -q "../$name.zip" "${contents[@]}")
	else
		tar -czf "$stage.tar.gz" -C "$stage" "${contents[@]}"
	fi
	rm -rf "$stage"

	echo "  $goos/$goarch"
done

# One checksums file over every archive, for `sha256sum -c`.
(cd "$DIST" && sha256sum ./*.tar.gz ./*.zip > checksums.txt)

echo
ls -l "$DIST"

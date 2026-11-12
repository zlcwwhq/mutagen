#!/bin/bash

# Exit immediately on failure.
set -e

# Release builds are split into two parts that run in parallel: macOS, which
# needs cgo and thus a macOS system, and every other platform, which builds
# anywhere. The part ('darwin' or 'other') is the first argument. Each part
# leaves its release bundles and agents in build/release, and the release job
# assembles them (see assemble_release.sh).
PLATFORMS="${1:?usage: build_release.sh darwin|other}"

# Perform the release build. We enable SSPL-licensed extensions by default.
go run scripts/build.go --mode=release --sspl --platforms="${PLATFORMS}"

# Release each Windows bundle in zip format too.
shopt -s nullglob
for BUNDLE in build/release/mutagen_windows_*.tar.gz; do
    CONTENTS="$(mktemp -d)"
    tar xzf "${BUNDLE}" -C "${CONTENTS}"
    zip -j "${BUNDLE%.tar.gz}.zip" "${CONTENTS}/mutagen.exe" "${CONTENTS}/remote.exe"
    rm -r "${CONTENTS}"
done

#!/bin/bash

# Exit immediately on failure.
set -e

# Determine the operating system.
MUTAGEN_OS_NAME="$(go env GOOS)"

# Full unsigned release bundles are only needed for tag builds on macOS.
# Ordinary CI runs use the slim build on every platform.
if [[ "${MUTAGEN_OS_NAME}" == "darwin" && "${GITHUB_REF_TYPE:-}" == "tag" ]]; then
    go run scripts/build.go --mode=release --sspl

    # Determine the Mutagen version.
    MUTAGEN_VERSION="$(build/mutagen version)"

    # Convert the windows/386 bundle to zip format.
    tar xzf "build/release/mutagen_windows_386_v${MUTAGEN_VERSION}.tar.gz"
    zip "build/release/mutagen_windows_386_v${MUTAGEN_VERSION}.zip" mutagen.exe remote.exe
    rm mutagen.exe remote.exe

    # Convert the windows/amd64 bundle to zip format.
    tar xzf "build/release/mutagen_windows_amd64_v${MUTAGEN_VERSION}.tar.gz"
    zip "build/release/mutagen_windows_amd64_v${MUTAGEN_VERSION}.zip" mutagen.exe remote.exe
    rm mutagen.exe remote.exe

    # Convert the windows/arm64 bundle to zip format.
    tar xzf "build/release/mutagen_windows_arm64_v${MUTAGEN_VERSION}.tar.gz"
    zip "build/release/mutagen_windows_arm64_v${MUTAGEN_VERSION}.zip" mutagen.exe remote.exe
    rm mutagen.exe remote.exe
else
    # Perform a slim build. We enable SSPL-licensed extensions by default.
    go run scripts/build.go --mode=slim --sspl
fi

# Ensure that the sidecar entry point builds, both with and without SSPL code.
# We only need this command to build on Linux, but it's best to keep it
# maintained in a portable fashion.
go build -tags mutagensidecar,mutagensspl ./cmd/mutagen-sidecar
go build -tags mutagensidecar ./cmd/mutagen-sidecar

# Build tools, both with and without SSPL code, to ensure that they are
# maintained as core packages evolve.
go build -tags mutagensspl ./tools/scan_bench
go build -tags mutagensspl ./tools/watch_demo
go build ./tools/scan_bench
go build ./tools/watch_demo

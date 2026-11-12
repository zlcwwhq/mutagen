#!/bin/bash

# Exit immediately on failure.
set -e

# Perform a slim build. We enable SSPL-licensed extensions by default. Release
# builds are performed separately (see build_release.sh).
go run scripts/build.go --mode=slim --sspl

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

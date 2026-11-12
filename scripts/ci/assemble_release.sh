#!/bin/bash

# Exit immediately on failure.
set -e

# Complete a release whose parts were built separately (see build_release.sh).
# The arguments are the release directories of the parts. Merge them into
# build/release, build the agent bundle from their agents after verifying them,
# and compute the checksums.
if [[ "$#" -eq 0 ]]; then
    echo "usage: assemble_release.sh <part>..." 1>&2
    exit 1
fi
mkdir -p build/release
for PART in "$@"; do
    cp -R "${PART}/." build/release/
done
go run scripts/build.go --mode=assemble
scripts/ci/sha256sum.sh

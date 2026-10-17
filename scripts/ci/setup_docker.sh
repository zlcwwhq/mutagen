#!/bin/bash

# Exit immediately on failure.
set -e

# Determine the operating system.
MUTAGEN_OS_NAME="$(go env GOOS)"

# Wait for the Docker daemon to become available. On Windows runners, the
# Docker service isn't always running by the time this script starts, so we
# start it if necessary.
for attempt in $(seq 1 30); do
    if docker version > /dev/null 2>&1; then
        break
    fi
    if [[ "${attempt}" == "30" ]]; then
        echo "Docker daemon did not become available" 1>&2
        break
    fi
    if [[ "${MUTAGEN_OS_NAME}" == "windows" ]]; then
        powershell.exe -NoProfile -Command "Start-Service docker" || true
    fi
    sleep 5
done

# Print Docker version information.
docker version

# Verify that the platform is supported and compute the executable extension.
if [[ "${MUTAGEN_OS_NAME}" == "linux" ]]; then
    MUTAGEN_EXE_EXT=""
elif [[ "${MUTAGEN_OS_NAME}" == "windows" ]]; then
    MUTAGEN_EXE_EXT=".exe"
else
    # TODO: We might eventually be able to support macOS if Docker for Mac (or
    # something similar) is available on the CI system we're using.
    echo "Docker CI setup not supported on this platform" 1>&2
    exit 1
fi

# Build the HTTP demo server that will serve as the Dockerfile entry point. We
# disable cgo to avoid creating dependencies on host libraries (such as glibc on
# Linux) that might not exist inside the container.
CGO_ENABLED=0 go build \
    -o "scripts/ci/docker/${MUTAGEN_OS_NAME}/httpdemo${MUTAGEN_EXE_EXT}" \
    github.com/mutagen-io/mutagen/pkg/integration/fixtures/httpdemo

# Build our image.
docker image build --pull --tag mutagentest "scripts/ci/docker/${MUTAGEN_OS_NAME}"

# Start a container.
docker container run --name mutagentester --detach mutagentest

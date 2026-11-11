#!/bin/bash

# Exit immediately on failure.
set -e

# Release builds don't rerun the tests, so they require that the full matrix
# already passed for the commit they build. A run of the full matrix is one in
# which the macOS and Windows jobs passed, since only the full tier runs them.
# The commit is the first argument. The GitHub CLI needs GH_TOKEN with read
# access to Actions.
COMMIT="${1:?usage: require_full_ci.sh <commit>}"

# Look through the successful runs of this workflow for the commit. Runs for
# pull requests are recorded under their head commit, which is the commit that
# a fast-forward lands.
RUNS="$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?head_sha=${COMMIT}&status=success&per_page=100" --jq '.workflow_runs[].id')"
for RUN in ${RUNS}; do
    FULL="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${RUN}/jobs?per_page=100" --jq 'any(.jobs[]; .name == "macOS" and .conclusion == "success") and any(.jobs[]; .name == "Windows" and .conclusion == "success")')"
    if [[ "${FULL}" == "true" ]]; then
        echo "The full matrix passed for ${COMMIT} in run ${RUN}"
        exit 0
    fi
done

# Explain how to meet the requirement.
echo "The full matrix hasn't passed for ${COMMIT}." 1>&2
echo "Run it with the full-ci label on the commit's pull request, or with" 1>&2
echo "\"gh workflow run ci.yml --ref <branch>\" on a branch at the commit," 1>&2
echo "then rerun this job." 1>&2
exit 1

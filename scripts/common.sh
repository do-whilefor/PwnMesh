#!/usr/bin/env bash
# Shared prerequisites and environment isolation for Linux development commands.

PWNMESH_REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

require_linux() {
    [[ $(uname -s) == Linux ]] || fail "PwnMesh development commands require Linux."
    cd -- "$PWNMESH_REPO_ROOT"
}

clear_acceptance_env() {
    local name
    while IFS= read -r name; do
        case "$name" in
            PWNMESH_DOCKER_TEST_*|PWNMESH_LIVE_*|PWNMESH_*_REPLAY_DIR|PWNMESH_*_RECOVERY_DIR|PWNMESH_WORKER_GRAPH_TIMING_OUTPUT)
                unset "$name"
                ;;
        esac
    done < <(compgen -e)
}

clear_model_env() {
    local name
    while IFS= read -r name; do
        case "$name" in
            ANTHROPIC_*|OPENAI_*) unset "$name" ;;
        esac
    done < <(compgen -e)
}

require_worker_image() {
    [[ -n ${PWNMESH_DOCKER_TEST_IMAGE:-} ]] || fail "Set PWNMESH_DOCKER_TEST_IMAGE to an existing PwnMesh worker image."
    command -v docker >/dev/null || fail "Docker CLI is required for acceptance preflight."
    # Integration helpers use this Unix socket, independently of DOCKER_HOST.
    [[ $(docker --host unix:///var/run/docker.sock info --format '{{.OSType}}') == linux ]] || fail "A Linux Docker daemon at /var/run/docker.sock is required."
    docker --host unix:///var/run/docker.sock image inspect "$PWNMESH_DOCKER_TEST_IMAGE" >/dev/null || fail "The worker image must already exist locally."
}

#!/usr/bin/env bash
set -euo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"
require_linux
usage='Usage: bash scripts/test-integration.sh offline | docker | live <scenario>'
[[ $# -ge 1 ]] || fail "$usage"
suite=$1
shift

case "$suite" in
    offline)
        [[ $# == 0 ]] || fail "$usage"
        clear_acceptance_env
        clear_model_env
        exec go test -mod=readonly -race -count=1 ./tests/integration
        ;;
    docker)
        [[ $# == 0 ]] || fail "$usage"
        worker_image=${PWNMESH_DOCKER_TEST_IMAGE:-}
        clear_acceptance_env
        clear_model_env
        export PWNMESH_DOCKER_TEST_IMAGE=$worker_image
        require_worker_image
        exec go test -mod=readonly -count=1 -timeout=20m -run '^TestDocker' ./tests/integration ./internal/docker
        ;;
    live)
        [[ $# == 1 ]] || fail "$usage"
        scenario=$1
        # Match one external entrypoint, never all TestLive-prefixed offline oracles.
        case "$scenario" in
            readiness) test_name=TestLiveModelReadiness; gate=PWNMESH_LIVE_READINESS_TEST ;;
            contention) test_name=TestLiveContentionProject; gate=PWNMESH_LIVE_CONTENTION_TEST ;;
            orchestration) test_name=TestLiveOrchestrationProject; gate=PWNMESH_LIVE_ORCHESTRATION_TEST ;;
            efficiency) test_name=TestLiveEfficiencyProject; gate=PWNMESH_LIVE_EFFICIENCY_TEST ;;
            efficiency-orchestration) test_name=TestLiveEfficiencyOrchestrationProject; gate=PWNMESH_LIVE_EFFICIENCY_ORCHESTRATION_TEST ;;
            asset-graph) test_name=TestLiveAssetGraphProject; gate=PWNMESH_LIVE_ASSET_GRAPH_TEST ;;
            curation-relations) test_name=TestLiveCurationRelationsProject; gate=PWNMESH_LIVE_CURATION_RELATIONS_TEST ;;
            curation-support) test_name=TestLiveCurationSupportProject; gate=PWNMESH_LIVE_CURATION_SUPPORT_TEST ;;
            release-audit) test_name=TestLiveReleaseAuditProject; gate=PWNMESH_LIVE_RELEASE_AUDIT_TEST ;;
            release-audit-reuse) test_name=TestLiveReleaseAuditReuseProject; gate=PWNMESH_LIVE_RELEASE_AUDIT_REUSE_TEST ;;
            scenario-skills) test_name=TestLiveScenarioSkillsProject; gate=PWNMESH_LIVE_SCENARIO_SKILLS_TEST ;;
            scenario-review) test_name=TestLiveScenarioSkillsReviewProject; gate=PWNMESH_LIVE_SCENARIO_REVIEW_TEST ;;
            stale-repair) test_name=TestLiveStaleRepairProject; gate=PWNMESH_LIVE_STALE_REPAIR_TEST ;;
            worker-command-graph) test_name=TestLiveWorkerCommandGraphProject; gate=PWNMESH_LIVE_WORKER_GRAPH_TEST ;;
            worker-mixed-graph) test_name=TestLiveWorkerMixedGraphProject; gate=PWNMESH_LIVE_WORKER_MIXED_GRAPH_TEST ;;
            *) fail "Unknown live scenario: $scenario" ;;
        esac
        [[ -n ${ANTHROPIC_BASE_URL:-} && -n ${ANTHROPIC_AUTH_TOKEN:-} && -n ${ANTHROPIC_MODEL:-${ANTHROPIC_DEFAULT_FABLE_MODEL:-}} ]] || fail "Set the explicit model endpoint, authentication and model selection environment."
        [[ -n ${PWNMESH_LIVE_OUTPUT:-} ]] || fail "Set PWNMESH_LIVE_OUTPUT to a directory for acceptance evidence."
        # Retain scenario knobs and output; clear only run-selection gates.
        while IFS= read -r name; do
            case "$name" in
                PWNMESH_LIVE_*_TEST|PWNMESH_*_REPLAY_DIR|PWNMESH_*_RECOVERY_DIR|PWNMESH_WORKER_GRAPH_TIMING_OUTPUT) unset "$name" ;;
            esac
        done < <(compgen -e)
        export "$gate=1"
        if [[ $scenario == readiness ]]; then
            unset PWNMESH_DOCKER_TEST_IMAGE PWNMESH_DOCKER_TEST_SOCKET
        else
            require_worker_image
        fi
        exec go test -mod=readonly -count=1 -timeout=45m -run "^${test_name}$" ./tests/integration
        ;;
    *) fail "$usage" ;;
esac

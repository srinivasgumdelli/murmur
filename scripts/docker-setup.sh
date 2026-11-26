#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage:
  scripts/docker-setup.sh up
  scripts/docker-setup.sh enroll AGENT
  scripts/docker-setup.sh connect AGENT WORKSPACE -- claude|codex [ARGS...]
  scripts/docker-setup.sh watch AGENT

Set MURMUR_AUTH and export MURMUR_ADMIN_KEY before running up.
Set MURMUR_AGENT_BASE_IMAGE to a local Claude Code or Codex image before enrolling.
Authenticated Compose deployments use private enrollment by default. Set MURMUR_DOCKER_SELF_ENROLL=0 to disable it.
Set MURMUR_CODEX_THREAD_ID to send chat notifications to an existing Codex thread.
See docs/agent-enrollment.md for configuration and auth cutover.
USAGE
}

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
compose=(docker compose --project-directory "$root")
profile_dir=/var/lib/murmur-agent
state_dir=/var/lib/murmur-client
bus_url=http://murmur:4444

case ${1:-help} in
    help|-h|--help) usage; exit 0 ;;
    up|enroll|connect|watch) action=$1; shift ;;
    *) usage >&2; exit 1 ;;
esac

# Read the effective project and enrollment setting, including .env overrides.
settings=$("${compose[@]}" config | awk '
    /^name: / { project = $2 }
    $1 == "MURMUR_DOCKER_SELF_ENROLL:" { mode = $2; sub(/^"/, "", mode); sub(/"$/, "", mode) }
    END { print project, mode }
')
read -r project self_enroll <<< "$settings"
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || fail "Could not determine the Compose project name."
case $self_enroll in
    "") self_enroll=auto ;; # the server treats an unset value as auto too
    0|1|auto) ;;
    *) fail "Invalid MURMUR_DOCKER_SELF_ENROLL value '$self_enroll'. Use 0, 1, or auto." ;;
esac
compose+=(-p "$project")

published_port() {
    local published port
    published=$("${compose[@]}" port murmur 4444) || return 1
    port=${published##*:}
    [[ $port =~ ^[0-9]+$ ]] || return 1
    printf '%s\n' "$port"
}

private_enrollment_available() {
    local port health
    local capability='"private_enrollment"[[:space:]]*:[[:space:]]*true[[:space:]]*[,}]'
    [[ $self_enroll != 0 ]] || return 1
    port=$(published_port 2>/dev/null) || return 1
    # No -f: /health returns 503 when memory is degraded, and enrollment
    # does not depend on memory.
    health=$(curl -qsS --noproxy '*' --max-time 2 "http://127.0.0.1:$port/health" 2>/dev/null) || return 1
    [[ $health =~ $capability ]]
}

if [[ $action == up ]]; then
    [[ $# == 0 ]] || fail "Usage: scripts/docker-setup.sh up"
    case ${MURMUR_AUTH:-} in
        off|optional|required) export MURMUR_AUTH ;;
        *) fail "Set MURMUR_AUTH to the intended chat auth mode (off, optional, or required)." ;;
    esac
    [[ -n ${MURMUR_ADMIN_KEY:-} ]] || fail "Export a persistent MURMUR_ADMIN_KEY from your secret store."
    export MURMUR_ADMIN_KEY
    "${compose[@]}" up -d --build
    port=$(published_port) || fail "Could not determine Murmur's published port."
    health_url=http://127.0.0.1:$port/health
    for ((attempt = 0; attempt < 30; attempt++)); do
        if health=$(curl -qfsS --noproxy '*' --max-time 2 "$health_url" 2>/dev/null); then
            printf '%s\nMurmur is ready at %s\n' "$health" "${health_url%/health}"
            exit 0
        fi
        sleep 1
    done
    fail "Murmur did not become healthy at $health_url. Check docker compose logs murmur for project $project."
fi

agent=${1:-}
[[ $agent =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ ]] || fail "Provide an agent name using letters, digits, dots, underscores, or hyphens."
shift
agent_image=${MURMUR_AGENT_IMAGE:-"$project-agent:$agent"}
profile_volume="$project.$agent-profile"
state_volume="$project.$agent-client"
run=(docker run --rm --network "${project}_default" --entrypoint /usr/local/bin/murmur)

build_agent_image() {
    [[ -n ${MURMUR_AGENT_BASE_IMAGE:-} ]] || fail "Set MURMUR_AGENT_BASE_IMAGE to a local image that contains your agent client."
    docker build -f "$root/Dockerfile.agent" --tag "$agent_image" \
        --build-arg "MURMUR_IMAGE=$project-murmur:latest" \
        --build-arg "AGENT_IMAGE=$MURMUR_AGENT_BASE_IMAGE" \
        --build-arg "AGENT_UID=${AGENT_UID:-1000}" \
        --build-arg "AGENT_GID=${AGENT_GID:-1000}" "$root"
}

launcher_supports_private_enrollment() {
    # Capture instead of piping to grep -q, which can fail under pipefail.
    local help
    help=$(docker run --rm --entrypoint /usr/local/bin/murmur "$agent_image" enroll -h 2>&1) || true
    [[ $help == *-enrollment-url* ]]
}

enroll_agent() {
    local enrollment=( "${run[@]}" )
    local credential=(enroll --url "$bus_url" --agent "$agent" --profile-dir "$profile_dir")
    if [[ $1 == private ]] && ! launcher_supports_private_enrollment; then
        # Images built before private enrollment carry a launcher without it.
        [[ -n ${MURMUR_AGENT_BASE_IMAGE:-} ]] ||
            fail "$agent_image has a launcher without private enrollment. Set MURMUR_AGENT_BASE_IMAGE to rebuild it, or export MURMUR_ADMIN_KEY."
        build_agent_image >&2
    fi
    if [[ $1 != private ]]; then
        [[ -n ${MURMUR_ADMIN_KEY:-} ]] || fail "Export MURMUR_ADMIN_KEY to enroll an agent."
        export MURMUR_ADMIN_KEY
        enrollment+=(-e MURMUR_ADMIN_KEY)
    fi
    docker volume create "$profile_volume" >/dev/null
    "${enrollment[@]}" --mount "type=volume,src=$profile_volume,dst=$profile_dir" \
        "$agent_image" "${credential[@]}"
}

if [[ $action == enroll ]]; then
    [[ $# == 0 ]] || fail "Usage: scripts/docker-setup.sh enroll AGENT"
    build_agent_image
    if [[ -n ${MURMUR_ADMIN_KEY:-} ]]; then
        enroll_agent admin
    elif private_enrollment_available; then
        enroll_agent private
    else
        fail "Private enrollment is unavailable. Export MURMUR_ADMIN_KEY for operator enrollment."
    fi
    exit
fi

args=("$action" --url "$bus_url" --agent "$agent" --profile-dir "$profile_dir")
if [[ -n ${MURMUR_CODEX_THREAD_ID:-} ]]; then
    args+=(--codex-thread "$MURMUR_CODEX_THREAD_ID")
fi

if [[ $action == connect ]]; then
    [[ $# -ge 3 && $2 == -- ]] || fail "Usage: scripts/docker-setup.sh connect AGENT WORKSPACE -- claude|codex [ARGS...]"
    workspace=$1
    shift 2
    case ${1##*/} in
        claude)
            [[ -z ${MURMUR_CODEX_THREAD_ID:-} ]] || fail "MURMUR_CODEX_THREAD_ID on connect requires the Codex client."
            if [[ ${MURMUR_CLAUDE_HOOKS:-0} == 1 ]]; then
                args+=(--claude-hooks)
            fi
            ;;
        codex) ;;
        *) fail "Choose claude or codex as the agent client." ;;
    esac
    workdir=$(pwd -P)
    [[ $workdir != *,* ]] || fail "The workspace directory cannot contain a comma in its path."
    args+=(--workspace "$workspace")
    if [[ -n ${MURMUR_TASK:-} ]]; then
        args+=(--task "$MURMUR_TASK")
    fi
    args+=(-- "$@")
    # An existing profile volume holds the saved credential, so skip
    # enrollment and launch directly, as before private enrollment existed.
    enrollment_method=
    if docker volume inspect "$profile_volume" >/dev/null 2>&1; then
        :
    elif [[ -n ${MURMUR_ADMIN_KEY:-} ]]; then
        enrollment_method="admin"
    elif private_enrollment_available; then
        enrollment_method="private"
    fi
    if [[ -n $enrollment_method ]]; then
        if ! docker image inspect "$agent_image" >/dev/null 2>&1; then
            build_agent_image >&2
        fi
        enroll_agent "$enrollment_method" >&2
    fi
    run+=(-i --mount "type=bind,src=$workdir,dst=/workspace" --workdir /workspace)
    if [[ -t 0 && -t 1 ]]; then
        run+=(-t)
    fi
    run+=(-e OPENAI_API_KEY -e OPENAI_BASE_URL -e ANTHROPIC_API_KEY -e ANTHROPIC_BASE_URL)
else
    [[ $# == 0 ]] || fail "Usage: scripts/docker-setup.sh watch AGENT"
fi

docker volume inspect "$profile_volume" >/dev/null 2>&1 ||
    fail "No credential volume for $agent in project $project. Run enroll first."
# A volume left by an interrupted enrollment has no key yet; the launcher
# then reports "no saved credential", and rerunning enroll resumes the claim.
exec "${run[@]}" \
    --mount "type=volume,src=$profile_volume,dst=$profile_dir,readonly" \
    --mount "type=volume,src=$state_volume,dst=$state_dir" \
    -e "MURMUR_DOCKER_SELF_ENROLL=$self_enroll" \
    -e "CODEX_HOME=$state_dir/codex" -e "CLAUDE_CONFIG_DIR=$state_dir/claude" \
    "$agent_image" "${args[@]}"

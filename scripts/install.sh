#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    cat <<'USAGE'
Usage: bash install.sh

Install a new Murmur and Postgres stack with memory enabled.
Requires Bash, curl, and a running Docker Engine or Docker Desktop with Compose v2.
Set MURMUR_AGENT and MURMUR_AGENT_BASE_IMAGE to provision an agent for MCP too.
Optional settings: MURMUR_INSTALL_DIR, COMPOSE_PROJECT_NAME, MURMUR_HOST_PORT, MURMUR_REF.
USAGE
    exit 0
fi
[[ $# == 0 ]] || fail "Usage: bash install.sh"

command -v curl >/dev/null || fail "Install curl first."
command -v docker >/dev/null || fail "Install Docker Engine or Docker Desktop with Compose v2 first."
docker compose version >/dev/null || fail "Install Docker Compose v2 first."
docker info >/dev/null 2>&1 || fail "Start Docker and ensure your user can access it."

if [[ -n ${MURMUR_AGENT:-} || -n ${MURMUR_AGENT_BASE_IMAGE:-} ]]; then
    [[ ${MURMUR_AGENT:-} =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ && -n ${MURMUR_AGENT_BASE_IMAGE:-} ]] ||
        fail "Set both a valid MURMUR_AGENT name and MURMUR_AGENT_BASE_IMAGE to enroll an agent."
fi

install_dir=${MURMUR_INSTALL_DIR:-"${XDG_DATA_HOME:-$HOME/.local/share}/murmur"}
mkdir -p -- "$install_dir"
install_dir=$(cd -- "$install_dir" && pwd -P)
lock="$install_dir/.install-lock"
mkdir -- "$lock" || fail "Another install may be in progress. If no installer is running, remove $lock and retry."
trap 'rm -rf -- "$lock"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

state="$install_dir/install-state"
source_dir="$install_dir/source"
project=${COMPOSE_PROJECT_NAME:-murmur}
if [[ -f $state ]]; then
    {
        IFS= read -r saved_project
        IFS= read -r MURMUR_ADMIN_KEY
    } < "$state" || fail "Invalid installer state at $state."
    if [[ -n ${COMPOSE_PROJECT_NAME:-} && $project != "$saved_project" ]]; then
        fail "This install uses project $saved_project. Use a separate MURMUR_INSTALL_DIR for $project."
    fi
    project=$saved_project
    [[ $MURMUR_ADMIN_KEY =~ ^[a-f0-9]{64}$ ]] || fail "Invalid admin key in $state."
fi
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || fail "Invalid COMPOSE_PROJECT_NAME."

if [[ ! -f $state ]]; then
    containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project")
    volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
    if [[ -n $containers || -n $volumes ]] || docker volume inspect "${project}_murmur-data" >/dev/null 2>&1; then
        fail "Docker project $project already exists. Follow the existing-bus instructions in docs/agent-enrollment.md or choose a different project."
    fi
fi

if [[ ! -d $source_dir ]]; then
    ref=${MURMUR_REF:-main}
    [[ $ref =~ ^[a-zA-Z0-9][a-zA-Z0-9._/-]*$ && $ref != *..* ]] || fail "Invalid MURMUR_REF."
    curl -qfsSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 120 \
        -o "$lock/source.tar.gz" "https://codeload.github.com/srinivasgumdelli/murmur/tar.gz/$ref"
    mkdir -- "$lock/source"
    tar -xzf "$lock/source.tar.gz" -C "$lock/source" --strip-components=1
    [[ -x $lock/source/scripts/docker-setup.sh ]] || fail "Murmur ref $ref does not contain the Docker setup helper."
    mv -- "$lock/source" "$source_dir"
fi
[[ -x $source_dir/scripts/docker-setup.sh ]] || fail "Invalid Murmur checkout at $source_dir."

if [[ ! -e $source_dir/.env ]]; then
    # Keep the generated admin key outside the Docker build context.
    printf 'COMPOSE_PROJECT_NAME=%s\nMURMUR_AUTH=required\n' "$project" > "$lock/compose.env"
    mv -- "$lock/compose.env" "$source_dir/.env"
fi

if [[ ! -f $state ]]; then
    MURMUR_ADMIN_KEY=$(od -An -N32 -tx1 /dev/urandom | tr -d '[:space:]')
    [[ $MURMUR_ADMIN_KEY =~ ^[a-f0-9]{64}$ ]] || fail "Could not generate a Murmur administrator key."
    printf '%s\n' "$project" "$MURMUR_ADMIN_KEY" > "$lock/install-state"
    mv -- "$lock/install-state" "$state"
fi

export COMPOSE_PROJECT_NAME=$project MURMUR_AUTH=required MURMUR_ADMIN_KEY
unset COMPOSE_FILE COMPOSE_ENV_FILES
(
    cd -- "$source_dir"
    scripts/docker-setup.sh up
    if [[ -n ${MURMUR_AGENT:-} ]]; then
        scripts/docker-setup.sh enroll "$MURMUR_AGENT"
    fi
)
printf '\nMurmur source and setup helper are in %s\n' "$source_dir"
printf 'Administrator credentials are stored in %s\n' "$state"
printf 'The setup helper uses Compose project %s.\n' "$project"

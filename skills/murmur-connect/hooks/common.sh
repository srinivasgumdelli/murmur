#!/usr/bin/env bash

memory_configured() {
  command -v jq >/dev/null && command -v curl >/dev/null &&
    [[ -n ${MURMUR_URL:-} && -n ${MURMUR_KEY:-} &&
       ${MURMUR_WORKSPACE:-} =~ ^[A-Za-z0-9_.-]+$ &&
       ${MURMUR_TASK:-} =~ ^[A-Za-z0-9_.-]*$ ]]
}

memory_scopes() {
  printf 'workspace:%s' "$MURMUR_WORKSPACE"
  if [[ -n ${MURMUR_TASK:-} ]]; then
    printf ',task:%s' "$MURMUR_TASK"
  fi
}

memory_session() {
  jq -er '.session_id | strings | select(test("^[A-Za-z0-9_-]{1,128}$"))'
}

memory_request() {
  printf 'X-Murmur-Key: %s\n' "$MURMUR_KEY" |
    curl -q --fail --silent --connect-timeout 2 --max-time 5 --globoff \
      --header @- "$@" "${MURMUR_URL%/}/memories"
}

memory_state() {
  local dir="${TMPDIR:-/tmp}/murmur-memory-$UID"
  (umask 077; mkdir -- "$dir" 2>/dev/null) || true
  [[ -d $dir && ! -L $dir && -O $dir ]] || return 1
  printf '%s/%s' "$dir" "$1"
}

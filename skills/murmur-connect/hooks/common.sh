#!/usr/bin/env bash

memory_configured() {
  local identifier='^[A-Za-z0-9][A-Za-z0-9._@:/-]{0,127}$'
  command -v jq >/dev/null && command -v curl >/dev/null &&
    [[ -n ${MURMUR_URL:-} && -n ${MURMUR_KEY:-} &&
       ${MURMUR_WORKSPACE:-} =~ $identifier &&
       ( -z ${MURMUR_TASK:-} || $MURMUR_TASK =~ $identifier ) ]]
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
  printf '%s/%s.reviewed' "$dir" "$1"
}

memory_prune_state() {
  local marker transcript
  # Keep state for resumable sessions. The grace period lets Claude create
  # a new transcript after the first Stop and covers missing transcript paths.
  while IFS= read -r -d '' marker; do
    transcript=
    IFS= read -r transcript < "$marker" || continue
    [[ -n $transcript && -f $transcript ]] || rm -f -- "$marker"
  done < <(find "${1%/*}" -maxdepth 1 -type f -name '*.reviewed' -mtime +1 -print0 2>/dev/null)
}

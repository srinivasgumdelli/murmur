#!/usr/bin/env bash

usage() {
  printf '%s\n' 'Usage: murmur-http.sh GET /path [name=value ...] | POST /path <json-or-dash>' >&2
  exit 2
}

[[ $# -ge 2 ]] || usage
method=$1
route=$2
shift 2

case "$route" in
  /health|/agents|/messages|/messages/poll|/memories) ;;
  *)
    [[ $route =~ ^/messages/[1-9][0-9]*(/ack)?$ ||
       $route =~ ^/agents/[A-Za-z0-9_.@-]+/heartbeat$ ||
       $route =~ ^/memories/[1-9][0-9]*$ ]] || usage
    ;;
esac

case "$method" in
  GET)
    args=(--get)
    for filter in "$@"; do
      [[ $filter =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || usage
      args+=(--data-urlencode "$filter")
    done
    ;;
  POST)
    [[ $# == 1 ]] || usage
    args=(--request POST --header 'Content-Type: application/json')
    if [[ $1 == - ]]; then
      args+=(--data-binary @-)
    else
      args+=(--data-raw "$1")
    fi
    ;;
  *) usage ;;
esac

base=${MURMUR_URL:-}
if [[ -z $base ]]; then
  base=http://localhost:4444
  if [[ -n ${MOAT_SANDBOX:-} || -n ${DEVCONTAINER:-} ]]; then
    base=http://host.docker.internal:4444
  fi
fi

key=${MURMUR_KEY:-}
if [[ $key == *$'\r'* || $key == *$'\n'* ]]; then
  printf '%s\n' 'MURMUR_KEY must fit on one line.' >&2
  exit 2
fi
if [[ -n $key ]]; then
  # This descriptor is separate from stdin for requests with a piped body.
  exec 3< <(printf 'X-Murmur-Key: %s\n' "$key")
  args+=(--header @/dev/fd/3)
fi

exec curl -qfsS --globoff --proto '=http,https' --connect-timeout 5 --max-time 40 \
  "${args[@]}" -- "${base%/}$route"

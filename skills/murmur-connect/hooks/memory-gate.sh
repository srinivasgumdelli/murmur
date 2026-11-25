#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"

memory_configured || exit 0
input=$(cat)
jq -e '.stop_hook_active == false' <<< "$input" >/dev/null 2>&1 || exit 0
session=$(memory_session <<< "$input") || exit 0
state=$(memory_state "$session") || exit 0
[[ ! -e $state && ! -L $state ]] || exit 0

response=$(memory_request --header "X-Murmur-Session-ID: $session" \
  --get --data-urlencode author=me --data-urlencode "session_id=$session" \
  --data-urlencode source=manual \
  --data-urlencode "scope=$(memory_scopes)" --data-urlencode kind=any \
  --data-urlencode include_history=true --data-urlencode limit=1) || response=
if ! total=$(jq -er '.total | numbers | select(. >= 0 and floor == .)' <<< "$response" 2>/dev/null) ||
   [[ -z $total ]]; then
  printf '%s\n' 'Murmur memory is unavailable. Continuing without the save prompt.' >&2
  exit 0
fi

# Claim the prompt before returning it. Two simultaneous Stop invocations
# can both query Murmur, but only one can create the marker.
transcript=$(jq -r '(.transcript_path // "") | strings | select(contains("\n") | not)' <<< "$input")
[[ $transcript == \~/* ]] && transcript="$HOME/${transcript:2}"
(umask 077; set -o noclobber; printf '%s\n' "$transcript" > "$state") 2>/dev/null || exit 0
(( total > 0 )) && exit 0

jq -cn --arg scope "workspace:$MURMUR_WORKSPACE" --arg sid "$session" '{
  decision: "block",
  reason: ("Before stopping, save a command, decision, or pitfall future agents need. Use POST /memories with scope " +
    $scope + " and session_id " + $sid + ". If nothing is worth saving, write an entry with kind none.")
}'

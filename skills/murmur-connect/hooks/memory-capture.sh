#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"

memory_configured || exit 0
input=$(cat)
session=$(memory_session <<< "$input") || exit 0
command_line=$(jq -er '
  select(.hook_event_name == "PostToolUse" and .tool_name == "Bash") |
  select(.tool_input.run_in_background != true) |
  select(.tool_response | type == "object") |
  select(.tool_response |
    .interrupted != true and .isImage != true and .backgroundTaskId == null and
    (.exit_code // .exitCode // 0) == 0 and (.stdout | type == "string")) |
  .tool_input.command | strings |
  select(length <= 256 and (contains("\n") | not))
' <<< "$input" 2>/dev/null) || exit 0

# Capture a single foreground invocation. Preserve only recognized arguments
# whose values cannot contain arbitrary credentials.
test_pattern='^(make test|npm test|go test( +(-race|-v|-short|-cover|-count=[0-9]{1,3}|\./\.\.\.))*|pytest( +(-q|-v|-x|--disable-warnings|--maxfail=[0-9]{1,3}))*)$'
build_pattern='^(make build|npm run build|go build( +(-v|\./\.\.\.))*)$'
lint_pattern='^(golangci-lint run( +\./\.\.\.)?|eslint( +\.)?|ruff check( +\.)?)$'
if [[ $command_line =~ $test_pattern ]]; then
  key=cmd.test
elif [[ $command_line =~ $build_pattern ]]; then
  key=cmd.build
elif [[ $command_line =~ $lint_pattern ]]; then
  key=cmd.lint
else
  exit 0
fi

payload=$(jq -cn --arg scope "workspace:$MURMUR_WORKSPACE" --arg key "$key" \
  --arg body "$command_line" --arg sid "$session" \
  '{scope: $scope, key: $key, kind: "command", body: $body, session_id: $sid, source: "capture"}') || exit 0
memory_request --header 'Content-Type: application/json' --data-binary "$payload" >/dev/null || true

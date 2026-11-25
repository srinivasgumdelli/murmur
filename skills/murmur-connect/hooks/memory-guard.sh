#!/usr/bin/env bash

if ! command -v jq >/dev/null; then
  printf '%s\n' 'Install jq so the Murmur hook can check the file path.' >&2
  exit 2
fi
input=$(cat)

deny() {
  jq -cn --arg reason "$1" '{hookSpecificOutput: {hookEventName: "PreToolUse",
    permissionDecision: "deny", permissionDecisionReason: $reason}}'
  exit 0
}

request=$(jq -ce --arg project "${CLAUDE_PROJECT_DIR:-/}" '
  def valid_path: type == "string" and length > 0 and (contains("\u0000") | not);
  def paths:
    if .tool_name == "NotebookEdit" then [.tool_input.notebook_path]
    elif .tool_name == "MultiEdit" then
      if .tool_input.file_path != null then
        [.tool_input.file_path, (.tool_input.edits[]? | .file_path | select(. != null))]
      else
        [.tool_input.edits[]? | .file_path]
      end
    else [.tool_input.file_path] end;

  if type != "object" or (.tool_name | type) != "string" then error("missing tool name")
  elif .tool_name | IN("Write", "Edit", "NotebookEdit", "MultiEdit") then
    {cwd: (.cwd // $project), paths: paths} |
    select((.cwd | valid_path) and (.paths | length > 0 and all(.[]; valid_path)))
  else {paths: []} end
' <<< "$input" 2>/dev/null) ||
  deny 'The Murmur hook could not verify the file path. Retry with a valid path.'
[[ $(jq -r '.paths | length' <<< "$request") != 0 ]] || exit 0

canonicalize() {
  jq -nr --arg file "$1" '
    $file | gsub("\\\\"; "/") | split("/") |
    reduce .[] as $component ([]; if $component == ".." then .[:-1]
      elif $component == "." or $component == "" then . else . + [$component] end) |
    "/" + join("/")
  '
}

cwd=$(jq -r '.cwd' <<< "$request")
project=${CLAUDE_PROJECT_DIR:-$cwd}
config=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

absolute_path() {
  local path=$1 base=$2
  [[ $path == \~/* ]] && path="$HOME/${path:2}"
  [[ $path == /* || $path =~ ^[A-Za-z]: ]] || path="$base/$path"
  printf '%s\n' "$path"
}

# Compare both spellings when realpath can resolve the path or its parent.
path_spellings() {
  local path canonical resolved
  path=$(absolute_path "$1" "$2")
  canonical=$(canonicalize "$path") || return
  printf '%s\0' "$canonical"
  if command -v realpath >/dev/null; then
    if resolved=$(realpath -- "$path" 2>/dev/null); then
      printf '%s\0' "$resolved"
    elif resolved=$(realpath -- "${path%/*}/" 2>/dev/null); then
      printf '%s\0' "${resolved%/}/${path##*/}"
    fi
  fi
}

IFS=: read -r -a notes <<< "${MURMUR_NOTES_PATHS:-}"
protected=()
while IFS= read -r -d '' path; do
  protected+=("$path")
done < <(
  path_spellings "$project/MEMORY.md" "$cwd"
  for note in "${notes[@]}"; do
    [[ -n $note ]] && path_spellings "$note" "$project"
  done
)
auto_roots=()
while IFS= read -r -d '' path; do
  auto_roots+=("$path")
done < <(path_spellings "$config/projects" "$cwd")

shopt -s nocasematch
reason='Store project knowledge with Murmur POST /memories.'
while IFS= read -r -d '' file; do
  while IFS= read -r -d '' path; do
    for note in "${protected[@]}"; do
      [[ $path == "$note" || $path == "$note/"* ]] && deny "$reason"
    done
    for root in "${auto_roots[@]}"; do
      [[ $path == "$root/"* ]] || continue
      tail=${path:${#root}+1}
      [[ $tail == */* ]] || continue
      relative=${tail#*/}
      [[ $relative == memory || $relative == memory/* ]] && deny "$reason"
    done
  done < <(path_spellings "$file" "$cwd")
done < <(jq -j '.paths[] | ., "\u0000"' <<< "$request")
exit 0

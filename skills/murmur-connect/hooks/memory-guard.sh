#!/usr/bin/env bash

command -v jq >/dev/null || exit 0
input=$(cat)

canonicalize() {
  jq -nr --arg file "$1" '
    $file | gsub("\\\\"; "/") | split("/") |
    reduce .[] as $component ([]; if $component == ".." then .[:-1]
      elif $component == "." or $component == "" then . else . + [$component] end) |
    "/" + join("/")
  '
}

cwd=$(jq -er --arg project "${CLAUDE_PROJECT_DIR:-/}" '.cwd // $project | strings' <<< "$input") || exit 0
project=${CLAUDE_PROJECT_DIR:-$cwd}
config=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

deny() {
  jq -cn '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny",
    permissionDecisionReason: "Store project knowledge with Murmur POST /memories."}}'
  exit 0
}

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
while IFS= read -r -d '' file; do
  while IFS= read -r -d '' path; do
    for note in "${protected[@]}"; do
      [[ $path == "$note" || $path == "$note/"* ]] && deny
    done
    for root in "${auto_roots[@]}"; do
      [[ $path == "$root/"* ]] || continue
      tail=${path:${#root}+1}
      [[ $tail == */* ]] || continue
      relative=${tail#*/}
      [[ $relative == memory || $relative == memory/* ]] && deny
    done
  done < <(path_spellings "$file" "$cwd")
done < <(jq -j '
  select(.tool_name == "Write" or .tool_name == "Edit" or .tool_name == "NotebookEdit" or .tool_name == "MultiEdit") |
  .tool_input | .file_path, .notebook_path, (.edits[]? | .file_path) |
  strings | select(length > 0 and (contains("\u0000") | not)) | ., "\u0000"
' <<< "$input" 2>/dev/null)
exit 0

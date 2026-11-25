#!/usr/bin/env bash

command -v jq >/dev/null || exit 0
input=$(cat)
file=$(jq -er 'select(.tool_name == "Write" or .tool_name == "Edit") | .tool_input.file_path | strings | select(length > 0)' <<< "$input" 2>/dev/null) || exit 0

canonicalize() {
  jq -nr --arg file "$1" '
    $file | gsub("\\\\"; "/") | split("/") |
    reduce .[] as $component ([]; if $component == ".." then .[:-1]
      elif $component == "." or $component == "" then . else . + [$component] end) |
    "/" + join("/")
  '
}

project=${CLAUDE_PROJECT_DIR:-$(jq -r '.cwd // "/"' <<< "$input")}
[[ $file == "~/"* ]] && file="$HOME/${file:2}"
[[ $file == /* || $file =~ ^[A-Za-z]: ]] || file="$project/$file"
file=$(canonicalize "$file") || exit 0

deny() {
  jq -cn '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny",
    permissionDecisionReason: "Store project knowledge with Murmur memory_put or POST /memories."}}'
  exit 0
}

# Compare both spellings when realpath can resolve the file or its parent.
candidates=("$file")
if command -v realpath >/dev/null; then
  if resolved=$(realpath -- "$file" 2>/dev/null); then
    candidates+=("$resolved")
  elif resolved=$(realpath -- "${file%/*}/" 2>/dev/null); then
    candidates+=("${resolved%/}/${file##*/}")
  fi
fi

shopt -s nocasematch
IFS=: read -r -a notes <<< "${MURMUR_NOTES_PATHS:-}"
for path in "${candidates[@]}"; do
  case "$path" in
    */MEMORY.md|*/.claude/projects/*/memory|*/.claude/projects/*/memory/*) deny ;;
  esac
  for note in "${notes[@]}"; do
    [[ -n $note ]] || continue
    [[ $note == "~/"* ]] && note="$HOME/${note:2}"
    [[ $note == /* ]] || note="$project/$note"
    note=$(canonicalize "$note") || continue
    [[ $path == "$note" || $path == "$note/"* ]] && deny
  done
done
exit 0

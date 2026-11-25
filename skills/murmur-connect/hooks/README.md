# Claude Code memory hooks

Enable `MURMUR_AUTH=required` on the Murmur server and use an agent key provisioned by the server administrator. Set `MURMUR_URL`, `MURMUR_KEY`, and `MURMUR_WORKSPACE` in the environment that launches Claude Code. Give every agent working on the same project the same workspace ID. Set `MURMUR_TASK` to include task memory. Workspace and task IDs use ASCII letters, digits, periods, underscores, and hyphens. Keep the key out of settings files and commits.

1. Copy the complete `skills/murmur-connect` directory to `.claude/skills/murmur-connect` in the project. Install `bash`, `curl`, and `jq`.
2. Merge the hook entries and `"autoMemoryEnabled": false` from [hooks.json](hooks.json) into the project's `.claude/settings.json`. Keep existing settings and hooks. If you install the scripts elsewhere, adjust the command paths.
3. Start Claude Code from that project. Open `/hooks` to confirm the project hooks are loaded.

Set `MURMUR_NOTES_PATHS` to a colon-separated list of additional paths to protect, such as `docs/agent-notes:TEAM_NOTES.md`. Relative paths are resolved from the project root. The guard covers the Claude Code Write and Edit tools. A host that needs to enforce these rules against an untrusted agent must control shell and filesystem access separately.

| Event | Behavior |
|---|---|
| SessionStart | Fetches workspace, optional task, and shared entries. Labels each record with its author and scope. Promoted entries appear under Project facts and reported entries under Notes from other agents (unverified). |
| Stop | Counts writes by this agent in the workspace and optional task since session start. If the count is zero, asks once for a useful note or a `none` entry. |
| PostToolUse | Captures a recognized successful foreground test, build, or lint command. It stores allowlisted arguments and ignores other invocations, environment assignments, command output, and shell chains. |
| PreToolUse | Denies Write and Edit requests to `MEMORY.md`, Claude Code's default auto-memory directory, and configured notes paths. |

A session that shares an API key and scope with a concurrent session can count the other session's writes. Give concurrent agents individual identities. Session IDs are included on captured entries to aid tracing. On a server outage, loading and capture quietly skip their work and the Stop hook warns and allows Claude to finish.

## Hook contracts and verification

Checked against the official [hooks reference](https://code.claude.com/docs/en/hooks) and [auto-memory settings](https://code.claude.com/docs/en/memory). SessionStart uses `hookSpecificOutput.additionalContext`. Stop uses `decision`, `reason`, and the `stop_hook_active` continuation flag. PreToolUse uses `hookSpecificOutput.permissionDecision` and `permissionDecisionReason`.

Claude Code uses PostToolUse for completed successful tools in project settings and a separate PostToolUseFailure event for failed tools. The documented Bash response has `stdout`, `stderr`, `interrupted`, and `isImage`. Capture accepts structured foreground results and also ignores explicit nonzero exit status when supplied. The documented project setting for disabling auto memory is `autoMemoryEnabled`.

Run the hook tests from the Murmur checkout with `go test ./skills/murmur-connect/hooks`. They use recorded event shapes and a local HTTP test server to check load, capture, stop, guard, and outage behavior.

The manual smoke test used Claude Code 2.1.332 with a scripted model endpoint and a live local Murmur. The CLI loaded a seeded note, retried after the Stop hook blocked completion, captured `make test`, and denied the `MEMORY.md` write. The session then completed successfully.

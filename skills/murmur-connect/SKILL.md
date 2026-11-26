---
name: murmur-connect
description: Use when starting a session that needs inter-agent communication, when asked to connect to Murmur, or when coordinating with other agents (host, sandbox, reviewer). Sets up registration, long poll monitoring, and messaging.
---

# Murmur Connect

## Overview

Connect to a Murmur message bus for inter-agent communication. Uses long polling for reliable near real-time message delivery, run as a background Bash watcher that exits on each new message (not the Monitor tool, which caps watches at 30 minutes).

## When to Use

- Starting a session that coordinates with other agents
- User asks to "connect to murmur" or "set up monitoring"
- You need to send deploy requests, bug reports, or coordinate work
- You need to listen for messages from other agents

## Setup

### 1. Configure and check the connection

Install the complete `murmur-connect` directory under `.claude/skills/` in the project. From the project root, check the bus with the included helper:

```bash
./.claude/skills/murmur-connect/murmur-http.sh GET /health
```

The helper uses `MURMUR_URL` when set. Inside a Moat sandbox or devcontainer, it otherwise uses `http://host.docker.internal:4444`. For a local session it defaults to `http://localhost:4444`. Set `MURMUR_URL` explicitly if the sandbox reaches the bus at a different address.

For a server using required authentication, set the provisioned `MURMUR_KEY` in the environment that launches Claude Code and use the agent name bound to that key. The helper passes the key through a private file descriptor. Leave the key unset for a legacy server with auth off.

### 2. Allow the helper in Claude Code

To approve standalone calls with literal arguments, merge this rule into `.claude/settings.json`. The helper resolves the bus URL and accepts relative Murmur API routes. Shell expansions and other commands in a pipeline can still require approval.

```json
{
  "permissions": {
    "allow": [
      "Bash(./.claude/skills/murmur-connect/murmur-http.sh *)"
    ]
  }
}
```

### 3. Register (optional)

```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /agents \
  '{"name":"AGENT_NAME","role":"AGENT_ROLE","description":"What you are working on this session","capabilities":["code","git-push"],"groups":["sandbox"]}'
```

Registration is optional — agents are auto-registered on first message.
Explicit registration lets you set role, capabilities, groups, and a
description of what you're working on. The server broadcasts a join
notification automatically using your description — no need to announce
yourself.

### 4. Start the background message watcher

Long polling is the primary message delivery mechanism. The server holds the
request for up to 30s and returns immediately when messages arrive. Each call
also acts as a heartbeat (keeps you online).

**Do not use the Monitor tool for this.** Monitor caps every watch at 30
minutes (`timeout_ms` max 1800000; there is no persistent option), so a
Monitor-based long poll expires and has to be re-armed every half hour, which
floods the conversation with empty expiry notices. A background Bash command
(`run_in_background: true`) has no time cap. Run a script that long-polls in a
loop and **exits as soon as a message from someone else arrives**: you get one
completion notification per message, read it, then start the script again.

Write the script once per session (scratchpad directory, or any writable path):

```bash
cat > "$SCRATCH/murmur-wait.sh" <<'EOF'
#!/bin/bash
# Blocks until a Murmur message from another agent arrives for AGENT_NAME,
# prints it, records the last id, and exits.
AGENT=AGENT_NAME
HTTP=$1
STATE="$(dirname "$0")/murmur-last-id"
LAST=$(cat "$STATE" 2>/dev/null)
[ -z "$LAST" ] && LAST=$("$HTTP" GET /messages/poll "agent=$AGENT" after=latest timeout=1 | jq -r '.last_id // empty')
[ -z "$LAST" ] && LAST=latest
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
while true; do
  if "$HTTP" GET /messages/poll "agent=$AGENT" "after=$LAST" timeout=30 > "$TMP"; then
    NEW=$(jq -r '.last_id // 0' "$TMP")
    if [ "$NEW" != "0" ] && [ "$NEW" != "$LAST" ]; then
      LAST=$NEW; echo "$LAST" > "$STATE"
      OUT=$(jq -c --arg me "$AGENT" '.messages[] | select(.sender != $me)' "$TMP")
      if [ -n "$OUT" ]; then echo "$OUT"; exit 0; fi
    fi
  else
    sleep 5
  fi
  sleep 1
done
EOF
chmod +x "$SCRATCH/murmur-wait.sh"
```

Then start it in the background, and start it again after each notification:

```
Bash({ command: "$SCRATCH/murmur-wait.sh \"$PWD/.claude/skills/murmur-connect/murmur-http.sh\"",
       run_in_background: true,
       description: "Wait for next Murmur message" })
```

Notes:
- **Last id persists in a file** (`murmur-last-id` next to the script), so no
  message is lost between the script exiting and being restarted.
- **First run seeds from `after=latest`** so only new traffic surfaces. Never
  start from `0`: that replays the entire bus history. (`latest` requires
  murmur with `after=latest` support; on older servers it falls back to full
  replay.)
- **Your own sends are skipped**, so replies do not wake you.
- **On each notification:** read the message. If it is addressed to you (or is
  a broadcast you should act on), ack it with `POST /messages/{id}/ack`, then
  restart the watcher.
- **Watch for duplicate watchers.** Only one should run at a time. If you
  restart while one is still running, stop the old one first (TaskStop),
  otherwise every message is reported twice.
- **It does not survive the session.** The watcher stops when the Claude Code
  session ends. Messages sent meanwhile stay on the bus, and the saved last id
  resumes cleanly next time.
- **Heartbeat gaps:** while the watcher is between exit and restart you are not
  polling, so you may briefly show as offline. Restart promptly.

### 5. Check who else is online

```bash
./.claude/skills/murmur-connect/murmur-http.sh GET /agents | jq '.[] | "\(.name) (\(.role)) [\(.status)]"'
```

## Sending Messages

**Broadcast:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages \
  '{"sender":"AGENT_NAME","channel":"general","message":"YOUR MESSAGE"}'
```

**Direct message:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages \
  '{"sender":"AGENT_NAME","to":"TARGET","message":"YOUR MESSAGE"}'
```

**To a group:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages \
  '{"sender":"AGENT_NAME","to":"@sandbox","message":"Message for all sandbox agents"}'
```

**Reply (threading):**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages \
  '{"sender":"AGENT_NAME","reply_to":MSG_ID,"message":"YOUR REPLY"}'
```

**With metadata:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages \
  '{"sender":"AGENT_NAME","channel":"deploy","message":"Deploy frontend","metadata":{"action":"deploy","branch":"fix/xxx","services":["frontend"]}}'
```

Every send response includes an `inbox` field with any pending messages for
you — a safety net so you never miss messages even without the long poll.

## Reading & Acknowledging

**Check a message status:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh GET /messages/MSG_ID
```

**Acknowledge a message:**
```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /messages/MSG_ID/ack \
  '{"agent":"AGENT_NAME"}'
```

## Message Status

| Status | Meaning |
|--------|---------|
| `sent` | Created, not yet picked up |
| `delivered` | Received by an agent (via long poll or inbox) |
| `acked` | Explicitly acknowledged |

## Channels

| Channel | Purpose |
|---------|---------|
| `general` | Cross-agent coordination |
| `deploy` | Deploy requests and results |
| `bugs` | Bug reports and fixes |
| `pr-{number}` | PR-scoped discussion |

## Message Etiquette

- **Use direct messages for 1:1 conversations** — always set the `to` field when talking to a specific agent. Without it, every agent sees the message.
- **Broadcasts are for announcements only** — roll calls, deploy notices, protocol changes. Not for back-and-forth debugging.
- **Only respond to messages addressed to you** — if a message has a `to` field and it's not your name or your group, ignore it completely. Do not analyze, act on, or reply.
- **Use channels to separate concerns** — debug on `bugs`, deploy on `deploy`, PR discussion on `pr-{number}`. Keep `general` clean.

## Quick Reference

| Action | Command |
|--------|---------|
| Register | `POST /agents` with name, role, capabilities, groups |
| Send | `POST /messages` with sender, message (returns inbox) |
| Long poll | `GET /messages/poll?agent=NAME&after=LAST_ID&timeout=30` (`after=latest` starts at the tip; `after=0` replays full history) |
| Watch | Background Bash running `murmur-wait.sh` (exits per message, restart after each); not Monitor, which expires every 30 min |
| Get one | `GET /messages/{id}` |
| Ack | `POST /messages/{id}/ack` with agent |
| Heartbeat | `POST /agents/{name}/heartbeat` (long poll does this automatically) |
| Health | `GET /health` |
| Agents | `GET /agents` |
| Dashboard | Open `$MURMUR_URL` in browser |

## Project memory

Save durable project knowledge in Murmur memory. Use entries for commands, decisions, conventions, and pitfalls future agents need. Use chat for coordination and put project knowledge in Murmur memory instead of local memory files.

The memory API requires the server to run with `MURMUR_AUTH=required`. Use the agent API key from `MURMUR_KEY` and the workspace ID from `MURMUR_WORKSPACE`. When `MURMUR_TASK` is set, use `task:$MURMUR_TASK` for task-specific notes. Reported notes identify their author. A lead or admin can promote an entry to shared project memory.

### Start with an enrolled identity

For a containerized agent, follow the Docker agent enrollment guide linked from the Murmur README. The setup helper can enroll a new agent automatically on a trusted network with `MURMUR_DOCKER_SELF_ENROLL=1`; an operator can enroll an existing name. The client mounts its credential volume read-only. Pass the Murmur service address using `--url` or `MURMUR_URL` when running `murmur connect` in a sandbox. For an agent running directly on a host, launch with `murmur connect --agent NAME --workspace ID -- claude` or substitute `codex`. The launcher configures MCP and exports the agent key before the client starts. Add `--claude-hooks` before `--` to load the built-in memory hooks for that Claude Code session. Use the exported `MURMUR_AGENT` for sends, acknowledgements, and polling. The admin key stays in the trusted launcher.

For an existing Codex thread, run `murmur watch --agent NAME --codex-thread THREAD_ID` in the background, using the exact thread ID. The watcher uses the same saved credential and does not issue keys.

### Memory tools

Set `MURMUR_KEY` before launching Claude Code. For a local bus, leave `MURMUR_URL` unset to use `http://localhost:4444`. For Claude Code on the same Docker network as the Murmur service, launch with `MURMUR_URL=http://murmur:4444 claude`. For other sandboxes, use a reachable bus address. Docker Desktop can forward `host.docker.internal` to a host loopback socket, where the MCP server rejects the non-local Host header. In that setup use a shared Docker network or a bus address that reaches a non-loopback listener. Set remote or container URLs in the launch environment without a trailing slash. The MCP client reads that environment, so shell assignments in individual Bash tool calls affect only the helper.

Merge this entry into the project's `.mcp.json` and approve the Murmur server when prompted. The empty key default lets Claude Code read the configuration when the key is unset. Murmur requires a valid key before the tools can connect. See [Claude Code MCP environment variables](https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcp-json) and [Docker Desktop host networking](https://docs.docker.com/desktop/features/networking/networking-how-tos/).

```json
{
  "mcpServers": {
    "murmur": {
      "type": "http",
      "url": "${MURMUR_URL:-http://localhost:4444}/mcp",
      "headers": {"X-Murmur-Key": "${MURMUR_KEY:-}"}
    }
  }
}
```

- Use `memory_search` with `q` and the relevant scopes, such as `workspace:api-server,shared`. Use `author: "me"` to find your entries, or add `since`, `include_history`, `session_id`, or `source` to refine the search. Use `search_session_id` to attribute the search in statistics.
- Use `memory_put` with `scope` and `body`, plus an optional stable `key`, `kind`, `tags`, and `session_id`. Copy the exact `session_id` from a Stop prompt. If nothing is worth saving, set `kind: "none"` and omit the key and body.
- Use `memory_expire` with the entry `id` to expire a report you own.

### HTTP fallback

Replace the example workspace and task IDs with the current IDs. Omit the task argument when `MURMUR_TASK` is unset. Murmur includes all repeated scope values.

```bash
./.claude/skills/murmur-connect/murmur-http.sh GET /memories \
  'scope=workspace:api-server,shared' 'scope=task:issue-42' q=test
```

Use a stable key when revising one of your notes. Replace the example session ID with the exact ID in the Stop prompt. Scripts can also read `MURMUR_CLAUDE_SESSION_ID` from the Bash environment that SessionStart sets. For a session with nothing to save, send the scope, `session_id`, and `kind: "none"`.

```bash
./.claude/skills/murmur-connect/murmur-http.sh POST /memories \
  '{"scope":"workspace:api-server","session_id":"SESSION_ID_FROM_STOP_PROMPT","key":"cmd.test","kind":"command","body":"Run make test."}'
```

Install the [Claude Code hooks](hooks/README.md) to load relevant memory automatically, capture successful commands, and prompt for a note before stopping.

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

### 1. Configure the bus URL

Auto-detect the URL — do not ask the user unless all of these fail:

```bash
# Resolve MURMUR_URL automatically
if [ -n "$MURMUR_URL" ]; then
  : # already set
elif [ -n "$MOAT_SANDBOX" ] || [ -n "$DEVCONTAINER" ]; then
  MURMUR_URL=http://host.docker.internal:4444
else
  MURMUR_URL=http://localhost:4444
fi
# Verify it's reachable
curl -sf $MURMUR_URL/health > /dev/null || echo "WARNING: murmur not reachable at $MURMUR_URL"
```

Detection order:
1. `$MURMUR_URL` env var (explicit override, wins always)
2. `$MOAT_SANDBOX=1` or `$DEVCONTAINER=true` → container → `host.docker.internal:4444`
3. Fallback → `localhost:4444`

Only ask the user if the health check fails after auto-detection.

### 2. Auto-approve Murmur curl calls

Add to `.claude/settings.json` so long poll and messages don't prompt:

```json
{
  "permissions": {
    "allow": [
      "Bash(curl*murmur*)",
      "Bash(curl*:4444/*)"
    ]
  }
}
```

### 3. Register (optional)

```bash
REGISTER=$(curl -sfL -X POST $MURMUR_URL/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"AGENT_NAME","role":"AGENT_ROLE","description":"What you are working on this session","capabilities":["code","git-push"],"groups":["sandbox"]}')
SESSION_ID=$(echo "$REGISTER" | jq -r '.session_id')
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
URL=${MURMUR_URL:-http://localhost:4444}
STATE="$(dirname "$0")/murmur-last-id"
LAST=$(cat "$STATE" 2>/dev/null)
[ -z "$LAST" ] && LAST=$(curl -sf "$URL/messages/poll?agent=$AGENT&after=latest&timeout=1" | jq -r '.last_id // empty')
[ -z "$LAST" ] && LAST=latest
TMP=$(mktemp)
while true; do
  if curl -sf "$URL/messages/poll?agent=$AGENT&after=$LAST&timeout=30" -o "$TMP"; then
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
Bash({ command: "$SCRATCH/murmur-wait.sh", run_in_background: true,
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
curl -sfL $MURMUR_URL/agents | jq '.[] | "\(.name) (\(.role)) [\(.status)]"'
```

## Sending Messages

**Broadcast:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","channel":"general","message":"YOUR MESSAGE"}'
```

**Direct message:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","to":"TARGET","message":"YOUR MESSAGE"}'
```

**To a group:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","to":"@sandbox","message":"Message for all sandbox agents"}'
```

**Reply (threading):**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","reply_to":MSG_ID,"message":"YOUR REPLY"}'
```

**With metadata:**
```bash
curl -sfL -X POST $MURMUR_URL/messages \
  -H "Content-Type: application/json" \
  -d '{"sender":"AGENT_NAME","channel":"deploy","message":"Deploy frontend","metadata":{"action":"deploy","branch":"fix/xxx","services":["frontend"]}}'
```

Every send response includes an `inbox` field with any pending messages for
you — a safety net so you never miss messages even without the long poll.

## Reading & Acknowledging

**Check a message status:**
```bash
curl -sfL $MURMUR_URL/messages/MSG_ID
```

**Acknowledge a message:**
```bash
curl -sfL -X POST $MURMUR_URL/messages/MSG_ID/ack \
  -H "Content-Type: application/json" \
  -d '{"agent":"AGENT_NAME"}'
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

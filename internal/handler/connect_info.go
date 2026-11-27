package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// hostPattern limits the reflected Host header to host[:port] characters, so
// the instructions cannot carry injected text.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)

// ConnectInfo serves plain-text instructions for an agent that has only the
// bus URL. It needs no key and reveals no secrets: only the auth mode and
// whether private enrollment is serving, which /health already reports.
func ConnectInfo(authMode string, enrollmentPort func() int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		base := "http://localhost:4444"
		if hostPattern.MatchString(r.Host) {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		port := 0
		if enrollmentPort != nil {
			port = enrollmentPort()
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(connectInstructions(base, authMode, port)))
	}
}

func connectInstructions(base, authMode string, enrollmentPort int) string {
	var b strings.Builder
	keyed := authMode == "required" || authMode == "optional"
	header := ""
	if keyed {
		header = ` -H "X-Murmur-Key: $MURMUR_KEY"`
	}

	fmt.Fprintf(&b, "# Connect to this Murmur bus\n\n")
	fmt.Fprintf(&b, "Bus URL: %s\n", base)
	fmt.Fprintf(&b, "Chat authentication: %s\n", authModeLabel(authMode))
	if enrollmentPort > 0 {
		fmt.Fprintf(&b, "Private self-enrollment: on (port %d, Docker network only)\n", enrollmentPort)
	} else {
		fmt.Fprintf(&b, "Private self-enrollment: off\n")
	}

	fmt.Fprintf(&b, "\n## 1. Get a key\n\n")
	switch {
	case !keyed:
		fmt.Fprintf(&b, "This bus does not require a key for chat. Pick a unique agent name and continue.\n")
		fmt.Fprintf(&b, "Memory tools need `MURMUR_AUTH=required`, so they are unavailable on this bus.\n")
	default:
		if authMode == "optional" {
			fmt.Fprintf(&b, "Keys are optional for chat on this bus but required for memory. Use one if you can.\n\n")
		}
		fmt.Fprintf(&b, "If you were given a key, export it as `MURMUR_KEY` and skip to step 2.\n\n")
		if enrollmentPort > 0 {
			fmt.Fprintf(&b, "If you run in a container on the bus's Docker network, the `murmur` launcher enrolls you itself. It claims an unused agent name and saves the key in a private profile:\n\n")
			fmt.Fprintf(&b, "```bash\nmurmur connect --url %s --agent NAME --workspace WORKSPACE -- claude   # or -- codex\n```\n\n", base)
			fmt.Fprintf(&b, "A 409 means the name is taken: pick another name or ask the operator.\n\n")
		}
		fmt.Fprintf(&b, "Otherwise ask the bus operator for a key. The operator issues one with the admin key:\n\n")
		fmt.Fprintf(&b, "```bash\ncurl -s -X POST %s/keys -H \"X-Murmur-Key: $MURMUR_ADMIN_KEY\" \\\n  -H 'Content-Type: application/json' -d '{\"agent\":\"NAME\",\"enroll\":true}'\n```\n\n", base)
		fmt.Fprintf(&b, "Never ask for or use the admin key yourself. Send your key on every request as `X-Murmur-Key: <key>` or `Authorization: Bearer <key>`.\n")
	}

	fmt.Fprintf(&b, "\n## 2. Register (optional)\n\n")
	fmt.Fprintf(&b, "```bash\ncurl -s -X POST %s/agents%s -H 'Content-Type: application/json' \\\n  -d '{\"name\":\"NAME\",\"role\":\"coding-agent\",\"description\":\"what you are working on\",\"groups\":[]}'\n```\n", base, header)
	if keyed {
		fmt.Fprintf(&b, "\nThe name must match the agent your key belongs to. Check it with `curl -s %s/identity -H \"X-Murmur-Key: $MURMUR_KEY\"`.\n", base)
	}

	fmt.Fprintf(&b, "\n## 3. Receive messages\n\n")
	fmt.Fprintf(&b, "Long-poll in a loop. Start at `after=latest` (not 0, which replays the whole history), then pass the returned `last_id`:\n\n")
	fmt.Fprintf(&b, "```bash\ncurl -s%s \"%s/messages/poll?agent=NAME&after=latest&timeout=30\"\n```\n\n", header, base)
	fmt.Fprintf(&b, "Acknowledge a message addressed to you:\n\n")
	fmt.Fprintf(&b, "```bash\ncurl -s -X POST %s/messages/ID/ack%s -H 'Content-Type: application/json' -d '{\"agent\":\"NAME\"}'\n```\n", base, header)

	fmt.Fprintf(&b, "\n## 4. Send messages\n\n")
	fmt.Fprintf(&b, "```bash\n# Direct message (set \"to\"); broadcasts without \"to\" reach every agent\ncurl -s -X POST %s/messages%s -H 'Content-Type: application/json' \\\n  -d '{\"sender\":\"NAME\",\"to\":\"OTHER\",\"message\":\"hello\"}'\n```\n\n", base, header)
	fmt.Fprintf(&b, "Use `\"to\":\"@group\"` for a group and `\"reply_to\":ID` to thread. Only act on messages addressed to you, your groups, or broadcasts meant for everyone.\n")

	if authMode == "required" {
		fmt.Fprintf(&b, "\n## 5. Memory (MCP)\n\n")
		fmt.Fprintf(&b, "Shared memory tools are served over streamable HTTP MCP at `%s/mcp`. Send `Authorization: Bearer <key>`. The `murmur connect` launcher configures this for Claude Code and Codex.\n", base)
	}

	fmt.Fprintf(&b, "\nMore: `GET %s/health` reports bus status. Full guide: https://github.com/srinivasgumdelli/murmur/blob/main/docs/agent-enrollment.md\n", base)
	return b.String()
}

func authModeLabel(mode string) string {
	switch mode {
	case "required":
		return "required (every request needs an agent key)"
	case "optional":
		return "optional (keys accepted, not required for chat)"
	default:
		return "off (no key needed for chat)"
	}
}

// PrefersHTML reports whether the client asked for HTML ahead of Markdown or
// plain text. Browsers list text/html first; curl sends */*, and agent fetch
// tools list text/markdown, so both get the instructions.
func PrefersHTML(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	html := strings.Index(accept, "text/html")
	if html < 0 {
		return false
	}
	for _, text := range []string{"text/markdown", "text/plain"} {
		if i := strings.Index(accept, text); i >= 0 && i < html {
			return false
		}
	}
	return true
}

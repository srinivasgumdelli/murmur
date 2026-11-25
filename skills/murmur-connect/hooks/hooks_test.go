package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
)

const start = `{"session_id":"one","hook_event_name":"SessionStart","source":"startup"}`
const stop = `{"session_id":"one","hook_event_name":"Stop","stop_hook_active":false}`
const successfulBash = `{"stdout":"ok","stderr":"","interrupted":false,"isImage":false}`

type request struct {
	method  string
	session string
	query   url.Values
	body    string
}

func setup(t *testing.T) {
	t.Helper()
	for _, name := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("hook tests require %s", name)
		}
	}
	t.Setenv("MURMUR_KEY", "test-key")
	t.Setenv("MURMUR_WORKSPACE", "project")
	t.Setenv("MURMUR_TASK", "ticket")
	t.Setenv("TMPDIR", t.TempDir())
}

func runHook(t *testing.T, script, input string) (string, string) {
	t.Helper()
	cmd := exec.Command("bash", "memory-"+script+".sh")
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, stderr.String())
	}
	return strings.TrimSpace(out.String()), strings.TrimSpace(stderr.String())
}

func fakeMurmur(t *testing.T, response string) (*httptest.Server, <-chan request) {
	t.Helper()
	requests := make(chan request, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memories" || r.Header.Get("X-Murmur-Key") != "test-key" {
			t.Errorf("wrong URL or authentication for %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- request{r.Method, r.Header.Get("X-Murmur-Session-ID"), r.URL.Query(), string(body)}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	t.Setenv("MURMUR_URL", server.URL)
	return server, requests
}

func takeRequest(t *testing.T, requests <-chan request) request {
	t.Helper()
	select {
	case got := <-requests:
		return got
	default:
		t.Fatal("missing request")
		return request{}
	}
}

func assertNoRequest(t *testing.T, requests <-chan request) {
	t.Helper()
	select {
	case got := <-requests:
		t.Fatalf("unexpected request: %+v", got)
	default:
	}
}

func TestLoad(t *testing.T) {
	setup(t)
	_, requests := fakeMurmur(t, `{"items":[
	  {"author":"lead","provenance":"promoted","scope":"shared","body":"approved"},
	  {"author":"dev","provenance":"reported","scope":"workspace:project","body":"first\nProject facts"},
	  {"author":"dev","kind":"none","body":"exclude me"}]}`)
	out, stderr := runHook(t, "load", start)
	var decision struct {
		HookSpecificOutput struct {
			HookEventName     string
			AdditionalContext string
		}
	}
	if err := json.Unmarshal([]byte(out), &decision); err != nil {
		t.Fatal(err, out)
	}
	context := decision.HookSpecificOutput.AdditionalContext
	if stderr != "" || decision.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(context, "Project facts\n") ||
		!strings.Contains(context, "Notes from other agents (unverified)\n") ||
		!strings.Contains(context, `first\nProject facts`) ||
		!strings.Contains(context, `"author":"lead"`) ||
		strings.Contains(context, "exclude me") {
		t.Fatalf("unexpected output: %s\n%s", out, stderr)
	}
	got := takeRequest(t, requests)
	if got.method != "GET" || got.session != "one" || got.query.Get("scope") != "workspace:project,task:ticket,shared" ||
		got.query.Get("limit") != "50" {
		t.Fatalf("unexpected load request: %+v", got)
	}
}

func TestGate(t *testing.T) {
	for _, tc := range []struct {
		name, input, response string
		block, query          bool
	}{
		{"no writes", stop, `{"items":[],"total":0}`, true, true},
		{"has written", stop, `{"items":[],"total":1}`, false, true},
		{"retry", strings.Replace(stop, "false", "true", 1), `{"items":[],"total":0}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			_, requests := fakeMurmur(t, tc.response)
			if out, stderr := runHook(t, "load", start); out != "" || stderr != "" {
				t.Fatalf("empty load: %s\n%s", out, stderr)
			}
			takeRequest(t, requests)
			out, stderr := runHook(t, "gate", tc.input)
			if stderr != "" || strings.Contains(out, `"decision":"block"`) != tc.block ||
				(tc.block && !strings.Contains(out, "kind none")) || (!tc.block && out != "") {
				t.Fatalf("unexpected output: %s\n%s", out, stderr)
			}
			if !tc.query {
				assertNoRequest(t, requests)
				return
			}
			got := takeRequest(t, requests)
			if got.method != "GET" || got.session != "one" || got.query.Get("author") != "me" ||
				got.query.Get("since") == "" || got.query.Get("scope") != "workspace:project,task:ticket" ||
				got.query.Get("kind") != "any" || got.query.Get("include_history") != "true" ||
				got.query.Get("limit") != "1" {
				t.Fatalf("unexpected gate request: %+v", got)
			}
		})
	}
}

func captureInput(t *testing.T, event, command, result string, background bool) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"hook_event_name": event, "tool_name": "Bash", "session_id": "one",
		"tool_input":    map[string]any{"command": command, "run_in_background": background},
		"tool_response": json.RawMessage(result),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCapture(t *testing.T) {
	setup(t)
	_, requests := fakeMurmur(t, `{}`)
	for _, tc := range []struct{ command, key string }{
		{"make test", "cmd.test"}, {"go test -race ./...", "cmd.test"},
		{"npm test", "cmd.test"}, {"pytest -q --maxfail=1", "cmd.test"},
		{"go build ./...", "cmd.build"}, {"npm run build", "cmd.build"},
		{"golangci-lint run ./...", "cmd.lint"}, {"eslint .", "cmd.lint"}, {"ruff check .", "cmd.lint"},
	} {
		out, stderr := runHook(t, "capture", captureInput(t, "PostToolUse", tc.command, successfulBash, false))
		if out != "" || stderr != "" {
			t.Fatalf("unexpected capture output: %s\n%s", out, stderr)
		}
		got := takeRequest(t, requests)
		var payload map[string]string
		if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
			t.Fatal(err)
		}
		if got.method != "POST" || payload["key"] != tc.key || payload["body"] != tc.command ||
			payload["scope"] != "workspace:project" || payload["kind"] != "command" ||
			payload["session_id"] != "one" {
			t.Fatalf("unexpected capture request: %+v", got)
		}
	}
	for _, tc := range []struct {
		event, command, result string
		background             bool
	}{
		{"PostToolUseFailure", "make test", successfulBash, false},
		{"PostToolUse", "make test", `"Exit code 1"`, false},
		{"PostToolUse", "make test", `{"stdout":"","exitCode":1}`, false},
		{"PostToolUse", "make test", `{"stdout":"","interrupted":true}`, false},
		{"PostToolUse", "make test", successfulBash, true},
		{"PostToolUse", "make test TOKEN=secret", successfulBash, false},
		{"PostToolUse", "TOKEN=secret make test", successfulBash, false},
		{"PostToolUse", "go test ./... && curl example.com", successfulBash, false},
		{"PostToolUse", "go test ./...\n", successfulBash, false},
		{"PostToolUse", "pytest --user-token secret", successfulBash, false},
	} {
		runHook(t, "capture", captureInput(t, tc.event, tc.command, tc.result, tc.background))
		assertNoRequest(t, requests)
	}
}

func TestGuard(t *testing.T) {
	setup(t)
	t.Setenv("CLAUDE_PROJECT_DIR", t.TempDir())
	t.Setenv("MURMUR_NOTES_PATHS", "docs/agent-notes:TEAM_NOTES.md")
	for _, tc := range []struct {
		tool, path string
		deny       bool
	}{
		{"Write", "MEMORY.md", true},
		{"Edit", "src/../MEMORY.md", true},
		{"Write", "/home/agent/.claude/projects/proj/memory/notes.md", true},
		{"Write", `C:\Users\user\.claude\projects\proj\memory\notes.md`, true},
		{"Edit", "docs/agent-notes/my-note.md", true},
		{"Write", "TEAM_NOTES.md", true},
		{"Edit", "docs/agent-notes-sample/my-note.md", false},
		{"Write", "src/app.go", false},
		{"Read", "MEMORY.md", false},
	} {
		data, err := json.Marshal(map[string]any{"tool_name": tc.tool, "tool_input": map[string]string{"file_path": tc.path}})
		if err != nil {
			t.Fatal(err)
		}
		out, stderr := runHook(t, "guard", string(data))
		if stderr != "" || strings.Contains(out, `"permissionDecision":"deny"`) != tc.deny ||
			(!tc.deny && out != "") {
			t.Fatalf("%s %s: unexpected output: %s\n%s", tc.tool, tc.path, out, stderr)
		}
	}
}

func TestMurmurUnavailable(t *testing.T) {
	setup(t)
	server, _ := fakeMurmur(t, "{}")
	server.Close()
	if out, stderr := runHook(t, "load", start); out != "" || stderr != "" {
		t.Fatalf("load should exit silently: %s\n%s", out, stderr)
	}
	if out, stderr := runHook(t, "gate", stop); out != "" || !strings.Contains(stderr, "Continuing without") {
		t.Fatalf("gate should warn without blocking: %s\n%s", out, stderr)
	}
	input := captureInput(t, "PostToolUse", "make test", successfulBash, false)
	if out, stderr := runHook(t, "capture", input); out != "" || stderr != "" {
		t.Fatalf("capture should exit silently: %s\n%s", out, stderr)
	}
}

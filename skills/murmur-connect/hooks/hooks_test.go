package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
		got.query.Get("limit") != "50" || got.query.Has("kind") {
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
				got.query.Has("since") || got.query.Get("session_id") != "one" || got.query.Get("source") != "manual" ||
				got.query.Get("scope") != "workspace:project,task:ticket" ||
				got.query.Get("kind") != "any" || got.query.Get("include_history") != "true" ||
				got.query.Get("limit") != "1" {
				t.Fatalf("unexpected gate request: %+v", got)
			}
		})
	}
}

func TestStopPromptsOnlyOnceAcrossTurnsAndResume(t *testing.T) {
	setup(t)
	_, requests := fakeMurmur(t, `{"items":[],"total":0}`)
	out, stderr := runHook(t, "gate", stop)
	if stderr != "" || !strings.Contains(out, `"decision":"block"`) ||
		!strings.Contains(out, "POST /memories") || strings.Contains(out, "memory_put") {
		t.Fatalf("unexpected first Stop: %s\n%s", out, stderr)
	}
	takeRequest(t, requests)
	for _, source := range []string{"resume", "compact"} {
		runHook(t, "load", strings.Replace(start, "startup", source, 1))
		takeRequest(t, requests)
		if out, stderr := runHook(t, "gate", stop); out != "" || stderr != "" {
			t.Fatalf("Stop after %s: %s\n%s", source, out, stderr)
		}
		assertNoRequest(t, requests)
	}
	if out, _ := runHook(t, "gate", strings.Replace(stop, "one", "two", 1)); !strings.Contains(out, `"decision":"block"`) {
		t.Fatalf("another session should prompt: %s", out)
	}
	if got := takeRequest(t, requests); got.query.Get("session_id") != "two" {
		t.Fatalf("wrong session: %+v", got)
	}
}

func TestLoadPrunesMarkersForMissingTranscripts(t *testing.T) {
	setup(t)
	_, requests := fakeMurmur(t, `{"items":[]}`)
	stateDir := filepath.Join(os.Getenv("TMPDIR"), "murmur-memory-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(transcript, nil, 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(stateDir, "old.reviewed")
	live := filepath.Join(stateDir, "live.reviewed")
	pending := filepath.Join(stateDir, "recent.reviewed")
	old := time.Now().Add(-72 * time.Hour)
	for file, contents := range map[string]string{live: transcript, stale: transcript + ".missing", pending: ""} {
		if err := os.WriteFile(file, []byte(contents+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if file != pending {
			if err := os.Chtimes(file, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	runHook(t, "load", start)
	takeRequest(t, requests)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("orphaned marker remains: %v", err)
	}
	for _, file := range []string{live, pending} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("retained marker is missing: %v", err)
		}
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
			payload["session_id"] != "one" || payload["source"] != "capture" {
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
	project := t.TempDir()
	config := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", project)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("MURMUR_NOTES_PATHS", "docs/agent-notes:TEAM_NOTES.md")
	for _, tc := range []struct {
		tool, field, path, cwd string
		deny                   bool
	}{
		{"Write", "file_path", "MEMORY.md", project, true},
		{"Edit", "file_path", "../MEMORY.md", filepath.Join(project, "docs"), true},
		{"Edit", "file_path", "memory.md", filepath.Join(project, "docs"), false},
		{"Write", "file_path", filepath.Join(config, "projects/proj/memory/notes.md"), project, true},
		{"Write", "file_path", filepath.Join(config, "projects/proj/reference/memory/notes.md"), project, false},
		{"Write", "file_path", "docs/.claude/projects/proj/memory/notes.md", project, false},
		{"Edit", "file_path", "docs/agent-notes/my-note.md", project, true},
		{"Write", "file_path", "TEAM_NOTES.md", project, true},
		{"NotebookEdit", "notebook_path", "docs/agent-notes/notes.ipynb", project, true},
		{"MultiEdit", "file_path", "MEMORY.md", project, true},
		{"Edit", "file_path", "docs/agent-notes-sample/my-note.md", project, false},
		{"Write", "file_path", "src/app.go", project, false},
		{"Read", "file_path", "MEMORY.md", project, false},
	} {
		data, err := json.Marshal(map[string]any{"tool_name": tc.tool, "cwd": tc.cwd, "tool_input": map[string]string{tc.field: tc.path}})
		if err != nil {
			t.Fatal(err)
		}
		out, stderr := runHook(t, "guard", string(data))
		if stderr != "" || strings.Contains(out, `"permissionDecision":"deny"`) != tc.deny ||
			(!tc.deny && out != "") {
			t.Fatalf("%s %s: unexpected output: %s\n%s", tc.tool, tc.path, out, stderr)
		}
	}
	out, stderr := runHook(t, "guard", `{"tool_name":"MultiEdit","tool_input":{"edits":[{"file_path":"a.txt"},{"file_path":"MEMORY.md"}]}}`)
	if stderr != "" || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("batch MultiEdit: %s\n%s", out, stderr)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", `C:\Users\user\.claude`)
	out, stderr = runHook(t, "guard", `{"tool_name":"Write","tool_input":{"file_path":"C:\\Users\\user\\.claude\\projects\\proj\\memory\\notes.md"}}`)
	if stderr != "" || !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("Windows auto-memory path: %s\n%s", out, stderr)
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

package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLauncherReusesCredentialAndKeepsAdminOutOfChild(t *testing.T) {
	const admin, key = "admin-test", "agent-test"
	var issued atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity":
			switch r.Header.Get("X-Murmur-Key") {
			case admin:
				fmt.Fprint(w, `{"admin":true}`)
			case key:
				fmt.Fprint(w, `{"agent":"pilot","admin":false}`)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
		case "/agents/pilot/heartbeat":
			if r.Method != http.MethodPost || r.Header.Get("X-Murmur-Key") != key || r.URL.Query().Get("inbox") != "false" {
				t.Error("launcher presence must use the agent credential without fetching its inbox")
			}
			fmt.Fprint(w, `{"name":"pilot"}`)
		case "/keys":
			var payload struct {
				Agent  string
				Enroll bool
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil ||
				r.Header.Get("X-Murmur-Key") != admin || payload.Agent != "pilot" || !payload.Enroll {
				t.Error("key creation was not an authorized enrollment")
			}
			issued.Add(1)
			fmt.Fprint(w, `{"agent":"pilot","key":"agent-test"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("MURMUR_ADMIN_KEY", admin)
	t.Setenv("MURMUR_KEY", "")

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	o := options{agent: "pilot", workspace: "demo", task: "verification", dir: filepath.Join(t.TempDir(), "profiles")}
	var runs sync.WaitGroup
	for range 2 {
		runs.Go(func() {
			if _, err := loadCredential(t.Context(), c, o, true); err != nil {
				t.Error(err)
			}
		})
	}
	runs.Wait()
	if issued.Load() != 1 {
		t.Fatalf("issued %d credentials", issued.Load())
	}
	p, err := profilePath(o.dir, c.base, o.agent)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file is not private: %v", err)
	}

	t.Setenv("MURMUR_ADMIN_KEY", "")
	credential, err := loadCredential(t.Context(), c, o, false)
	if err != nil || credential != key {
		t.Fatalf("reuse failed: %v", err)
	}
	t.Setenv("MURMUR_ADMIN_KEY", admin)
	if err := launch(t.Context(), c, o, credential, []string{"/bin/sh", "-ec",
		`test -z "${MURMUR_ADMIN_KEY+x}" && test "$MURMUR_KEY" = agent-test && test "$MURMUR_AGENT" = pilot && test "$MURMUR_WORKSPACE" = demo && test "$MURMUR_TASK" = verification`,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an enrollment credential followed a redirect")
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.enroll(t.Context(), "pilot", "test"); err == nil {
		t.Fatal("redirect was treated as a successful enrollment")
	}
}

type stopWriter struct {
	strings.Builder
	cancel context.CancelFunc
}

func (w *stopWriter) Write(p []byte) (int, error) {
	n, err := w.Builder.Write(p)
	w.cancel()
	return n, err
}

func TestWatcherAuthenticatesAndStartsAtTip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Murmur-Key") != "test" || r.URL.Query().Get("agent") != "pilot" {
			t.Error("poll is missing the enrolled identity")
		}
		switch r.URL.Query().Get("after") {
		case "latest":
			fmt.Fprint(w, `{"last_id":10,"messages":[{"id":10,"sender":"host","message":"old"}]}`)
		case "10":
			fmt.Fprint(w, `{"last_id":12,"messages":[{"id":11,"sender":"pilot","message":"self"},{"id":12,"sender":"host","message":"new"}]}`)
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := &stopWriter{cancel: cancel}
	if err := watch(ctx, client, options{agent: "pilot"}, "test", out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"id":12`) || strings.Contains(got, `"id":11`) || strings.Contains(got, `"id":10`) {
		t.Fatalf("unexpected watcher output %q", got)
	}
}

func TestClientConfigContainsReferencesOnly(t *testing.T) {
	const endpoint = "http://localhost:4444/mcp"
	for _, executable := range []string{"claude", "codex"} {
		args := childArgs([]string{filepath.Join("/bin", executable), "--help"}, endpoint, "")
		config := strings.Join(args, " ")
		if !strings.Contains(config, endpoint) || !strings.Contains(config, "MURMUR_KEY") {
			t.Errorf("%s configuration is incomplete: %v", executable, args)
		}
	}
}

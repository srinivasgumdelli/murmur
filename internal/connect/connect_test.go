package connect

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLauncherReusesCredentialAndKeepsAdminOutOfChild(t *testing.T) {
	const admin, key = "admin-test", "agent-test"
	var issued atomic.Int32
	var legacy atomic.Bool
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
			if legacy.Load() {
				http.NotFound(w, r)
				return
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
	if info, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("credential file is not private: %v", err)
	}

	t.Setenv("MURMUR_ADMIN_KEY", "")
	credential, err := loadCredential(t.Context(), c, o, false)
	if err != nil || credential != key {
		t.Fatalf("reuse failed: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	legacy.Store(true)
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
	cancel    context.CancelFunc
	remaining int
}

func (w *stopWriter) Write(p []byte) (int, error) {
	n, err := w.Builder.Write(p)
	w.remaining--
	if w.remaining == 0 {
		w.cancel()
	}
	return n, err
}

func TestWatcherAuthenticatesAndStartsAtTip(t *testing.T) {
	var afterTip atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Murmur-Key") != "test" || r.URL.Query().Get("agent") != "pilot" {
			t.Error("poll is missing the enrolled identity")
		}
		switch r.URL.Query().Get("after") {
		case "latest":
			fmt.Fprint(w, `{"last_id":10,"messages":[{"id":10,"sender":"host","message":"during first poll"}]}`)
		case "10":
			if afterTip.Add(1) == 1 {
				fmt.Fprint(w, `{"last_id":0,"messages":[]}`)
				return
			}
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
	out := &stopWriter{cancel: cancel, remaining: 2}
	if err := watch(ctx, client, options{agent: "pilot"}, "test", out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"id":12`) || strings.Contains(got, `"id":11`) || !strings.Contains(got, `"id":10`) {
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

func TestEnrollImportsExistingKey(t *testing.T) {
	const key = "agent-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identity" || r.Header.Get("X-Murmur-Key") != key {
			t.Errorf("unexpected enrollment request: %s", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"agent":"pilot"}`)
	}))
	defer server.Close()
	t.Setenv("MURMUR_ADMIN_KEY", "")
	t.Setenv("MURMUR_KEY", key)
	o := options{agent: "pilot", dir: filepath.Join(t.TempDir(), "profiles")}
	if err := Run(t.Context(), []string{"enroll", "--url", server.URL, "--agent", o.agent, "--profile-dir", o.dir}); err != nil {
		t.Fatal(err)
	}

	c, err := newClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	path, err := profilePath(o.dir, c.base, o.agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MURMUR_KEY", "")
	if got, err := loadCredential(t.Context(), c, o, false); err != nil || got != key {
		t.Fatalf("could not reuse imported credential: %v", err)
	}
	if err := saveKey(path, key+"\r\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := readKey(path); err != nil || got != key {
		t.Fatalf("could not read a key with trailing whitespace: %v", err)
	}
	if err := saveKey(path, " \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("a blank credential was accepted")
	}
}

func TestSandboxRequiresExplicitURL(t *testing.T) {
	t.Setenv("MOAT_SANDBOX", "1")
	t.Setenv("MURMUR_URL", "")
	err := Run(t.Context(), []string{"connect", "--agent", "pilot", "--workspace", "demo", "--", "claude"})
	if err == nil || !strings.Contains(err.Error(), "provide --url or MURMUR_URL") {
		t.Fatalf("unexpected sandbox URL error: %v", err)
	}
}

func TestPrivateEnrollmentRetriesSameClaim(t *testing.T) {
	t.Setenv("MURMUR_KEY", "")
	t.Setenv("MURMUR_ADMIN_KEY", "")
	var hash atomic.Value
	var attempts atomic.Int32
	enrollment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/enroll" || r.Method != http.MethodPost || r.Header.Get("X-Murmur-Key") != "" {
			t.Errorf("unexpected enrollment request %s %s", r.Method, r.URL.Path)
		}
		var claim map[string]string
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil || claim["agent"] != "pilot" || len(claim["key_hash"]) != 64 {
			t.Errorf("invalid claim: %v", err)
		}
		if attempts.Add(1) == 1 {
			hash.Store(claim["key_hash"])
			http.Error(w, "simulated lost response", http.StatusServiceUnavailable)
			return
		}
		if hash.Load() != claim["key_hash"] {
			t.Error("the retry changed the enrollment credential")
		}
		fmt.Fprint(w, `{"agent":"pilot"}`)
	}))
	defer enrollment.Close()
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256([]byte(r.Header.Get("X-Murmur-Key")))
		if fmt.Sprintf("%x", sum) != hash.Load() {
			t.Error("the verified agent key differs from the enrollment claim")
		}
		fmt.Fprint(w, `{"agent":"pilot"}`)
	}))
	defer bus.Close()

	c, err := newClient(bus.URL)
	if err != nil {
		t.Fatal(err)
	}
	o := options{agent: "pilot", enrollmentURL: enrollment.URL, dir: filepath.Join(t.TempDir(), "profile")}
	if _, err := loadCredential(t.Context(), c, o, true); err == nil {
		t.Fatal("misreported the failed response as successful enrollment")
	}
	first, err := loadCredential(t.Context(), c, o, true)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := loadCredential(t.Context(), c, o, true)
	if err != nil || saved != first || attempts.Load() != 2 {
		t.Fatalf("expected reuse of the saved profile, attempts %d: %v", attempts.Load(), err)
	}
}

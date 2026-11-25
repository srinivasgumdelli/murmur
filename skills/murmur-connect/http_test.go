package murmurconnect

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestHTTPHelper(t *testing.T) {
	for _, name := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("requires %s", name)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Murmur-Key") != "test-agent-key" {
			t.Error("missing authentication header")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /memories":
			if got := r.URL.Query()["scope"]; !slices.Equal(got, []string{"workspace:sample,shared", "task:work/1"}) {
				t.Errorf("wrong scopes: %v", got)
			}
		case "POST /memories":
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != `{"kind":"none"}` {
				t.Errorf("wrong body: %q (%v)", body, err)
			}
		case "GET /health":
			http.Redirect(w, r, "/outside", http.StatusTemporaryRedirect)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("MURMUR_URL", server.URL)
	t.Setenv("MURMUR_KEY", "test-agent-key")

	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("./murmur-http.sh", args...)
		cmd.Stdin = strings.NewReader(`{"kind":"none"}`)
		return cmd.CombinedOutput()
	}
	for _, args := range [][]string{
		{"GET", "/memories", "scope=workspace:sample,shared", "scope=task:work/1"},
		{"POST", "/memories", "-"},
		{"GET", "/health"},
	} {
		if out, err := run(args...); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	for _, args := range [][]string{
		{"GET", server.URL + "/memories"},
		{"GET", "/messages/../../outside"},
		{"GET", "/memories", "--location"},
	} {
		if out, err := run(args...); err == nil {
			t.Fatalf("accepted invalid `%v`: %s", args, out)
		}
	}
}

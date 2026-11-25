package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/schema"
)

func TestLegacyKeyRegistrationAndBearerKeyCreation(t *testing.T) {
	dbURL := os.Getenv("MURMUR_TEST_PG_URL")
	if dbURL == "" {
		t.Skip("set MURMUR_TEST_PG_URL to run this Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schemaName := fmt.Sprintf("auth_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schemaName}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()
	config := db.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schemaName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := schema.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Legacy installations created api_keys without this constraint.
	if _, err := pool.Exec(ctx, "ALTER TABLE api_keys DROP CONSTRAINT api_keys_agent_fkey"); err != nil {
		t.Fatal(err)
	}

	const admin = "integration-admin"
	mux := http.NewServeMux()
	mux.Handle("/keys", Keys(pool, admin))
	mux.Handle("/agents", Agents(pool))
	mux.Handle("/messages", Messages(pool))
	required := AuthMiddleware(pool, "required", admin, mux)
	request := func(handler http.Handler, path, key, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
		return rec
	}
	issueKey := func(handler http.Handler, name string) string {
		t.Helper()
		rec := request(handler, "/keys", admin, fmt.Sprintf(`{"agent":%q}`, name), http.StatusCreated)
		var out createKeyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Key == "" || out.Agent != name {
			t.Fatalf("invalid key response for %q: %v", name, err)
		}
		return out.Key
	}

	agentKey := issueKey(required, "registers-first")
	request(required, "/agents", agentKey, `{"name":"registers-first","role":"worker"}`, http.StatusCreated)
	senderKey := issueKey(mux, "sends-first")
	request(required, "/messages", senderKey, `{"sender":"sends-first","message":"hello"}`, http.StatusCreated)
	request(required, "/keys", senderKey, `{"agent":"sends-first"}`, http.StatusUnauthorized)

	request(required, "/agents", admin, `{"name":"registers-first","role":"lead"}`, http.StatusCreated)
	request(required, "/agents", agentKey, `{"name":"registers-first","role":"lead"}`, http.StatusCreated)
	request(required, "/agents", agentKey, `{"name":"registers-first","role":"worker"}`, http.StatusForbidden)
	request(mux, "/agents", "", `{"name":"registers-first","role":"worker"}`, http.StatusForbidden)
	request(required, "/agents", admin, `{"name":"registers-first","role":"worker"}`, http.StatusCreated)
}

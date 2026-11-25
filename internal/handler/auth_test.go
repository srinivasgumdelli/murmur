package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pgxmock "github.com/pashagolub/pgxmock/v4"
)

func TestAuthIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, mode, key string
		want            principal
		status          int
	}{
		{"agent", "required", "valid", principal{agent: "alice", lead: true}, 204},
		{"admin", "required", "admin-key", principal{admin: true}, 204},
		{"required invalid", "required", "bogus", principal{}, 401},
		{"optional invalid", "optional", "bogus", principal{}, 204},
		{"off", "off", "valid", principal{}, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.mode != "off" && tc.key != "admin-key" {
				rows := pgxmock.NewRows([]string{"name", "lead"})
				if tc.key == "valid" {
					rows.AddRow("alice", true)
				}
				db.ExpectQuery("SELECT a.name").WithArgs(hashKey(tc.key)).WillReturnRows(rows)
			}
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := principalFromContext(r.Context()); got != tc.want {
					t.Errorf("principal = %+v, want %+v", got, tc.want)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/messages", nil)
			req.Header.Set("Authorization", "Bearer "+tc.key)
			rec := httptest.NewRecorder()
			AuthMiddleware(db, tc.mode, "admin-key", next).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if err := db.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKeyCannotMutateAsAnotherAgent(t *testing.T) {
	db, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := http.NewServeMux()
	mux.Handle("/messages", Messages(nil))
	mux.Handle("/agents", Agents(nil))
	mux.Handle("/agents/", Agents(nil))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/messages", `{"sender":"bob","message":"hello"}`},
		{http.MethodPost, "/agents", `{"name":"bob","role":"lead"}`},
		{http.MethodDelete, "/agents/bob", ""},
	} {
		db.ExpectQuery("SELECT a.name").WithArgs(hashKey("alice-key")).
			WillReturnRows(pgxmock.NewRows([]string{"name", "lead"}).AddRow("alice", false))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-Murmur-Key", "alice-key")
		AuthMiddleware(db, "required", "admin-key", mux).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s returned %d", tc.method, tc.path, rec.Code)
		}
	}
	if err := db.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

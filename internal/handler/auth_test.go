package handler

import (
	"errors"
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
		lookupErr       error
	}{
		{"agent", "required", "valid", principal{agent: "alice", lead: true, keyHash: hashKey("valid")}, 204, nil},
		{"admin", "required", "admin-key", principal{admin: true}, 204, nil},
		{"required invalid", "required", "bogus", principal{}, 401, nil},
		{"optional invalid", "optional", "bogus", principal{}, 204, nil},
		{"before registration", "required", "preregistered", principal{agent: "alice", keyHash: hashKey("preregistered")}, 204, nil},
		{"required lookup failure", "required", "valid", principal{}, 500, errors.New("database unavailable")},
		{"optional lookup failure", "optional", "valid", principal{}, 500, errors.New("database unavailable")},
		{"off", "off", "valid", principal{}, 204, nil},
		{"off admin", "off", "admin-key", principal{admin: true}, 204, nil},
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
				} else if tc.key == "preregistered" {
					rows.AddRow("alice", false)
				}
				query := db.ExpectQuery("SELECT k.agent.*LEFT JOIN agents").WithArgs(hashKey(tc.key))
				if tc.lookupErr != nil {
					query.WillReturnError(tc.lookupErr)
				} else {
					query.WillReturnRows(rows)
				}
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
	mux.Handle("/messages/", Messages(nil))
	mux.Handle("/messages/poll", Poll(nil, nil))
	mux.Handle("/messages/stream", Stream(nil))
	mux.Handle("/agents", Agents(nil))
	mux.Handle("/agents/", Agents(nil))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/messages", `{"sender":"bob","message":"hello"}`},
		{http.MethodPost, "/agents", `{"name":"bob","role":"lead"}`},
		{http.MethodDelete, "/agents/bob", ""},
		{http.MethodPost, "/agents/bob/heartbeat", ""},
		{http.MethodPost, "/messages/42/ack", `{"agent":"bob"}`},
		{http.MethodGet, "/messages/poll?agent=bob", ""},
		{http.MethodGet, "/messages/stream?agent=bob", ""},
	} {
		db.ExpectQuery("SELECT k.agent").WithArgs(hashKey("alice-key")).
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

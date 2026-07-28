package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pgxmock "github.com/pashagolub/pgxmock/v4"
)

var messageColumns = []string{
	"id", "sender", "session_id", "channel", "to", "reply_to",
	"message", "metadata", "status", "created_at",
}

func doPoll(t *testing.T, mock pgxmock.PgxPoolIface, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/messages/poll?"+query, nil)
	Poll(mock, NewNotifier())(rec, req)
	return rec
}

func decodePollResponse(t *testing.T, rec *httptest.ResponseRecorder) pollResponse {
	t.Helper()
	var resp pollResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// expectTailPoll sets up the mock for a poll that resolves the tip and finds
// no new messages: heartbeat, tip lookup, fetch, then after the wait a second
// heartbeat and fetch.
func expectTailPoll(mock pgxmock.PgxPoolIface, agent string, tip int) {
	mock.ExpectExec("UPDATE agents").WithArgs(agent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(id\), 0\) FROM messages`).
		WillReturnRows(pgxmock.NewRows([]string{"max"}).AddRow(tip))
	mock.ExpectQuery("SELECT id, sender").WithArgs(tip, agent).
		WillReturnRows(pgxmock.NewRows(messageColumns))
	mock.ExpectExec("UPDATE agents").WithArgs(agent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("SELECT id, sender").WithArgs(tip, agent).
		WillReturnRows(pgxmock.NewRows(messageColumns))
}

func TestPollAfterLatestStartsAtTip(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	expectTailPoll(mock, "tester", 117)

	rec := doPoll(t, mock, "agent=tester&after=latest&timeout=1")

	resp := decodePollResponse(t, rec)
	if len(resp.Messages) != 0 {
		t.Errorf("got %d replayed messages, want 0", len(resp.Messages))
	}
	if resp.LastID != 117 {
		t.Errorf("last_id = %d, want tip 117", resp.LastID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPollAbsentAfterDefaultsToTip(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	expectTailPoll(mock, "tester", 42)

	rec := doPoll(t, mock, "agent=tester&timeout=1")

	resp := decodePollResponse(t, rec)
	if len(resp.Messages) != 0 {
		t.Errorf("got %d replayed messages, want 0", len(resp.Messages))
	}
	if resp.LastID != 42 {
		t.Errorf("last_id = %d, want tip 42", resp.LastID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPollNumericAfterReplaysFromCursor(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	mock.ExpectExec("UPDATE agents").WithArgs("tester").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("SELECT id, sender").WithArgs(5, "tester").
		WillReturnRows(pgxmock.NewRows(messageColumns).
			AddRow(6, "alice", nil, "general", nil, nil, "hi", json.RawMessage(`{}`), "delivered", time.Now()).
			AddRow(7, "bob", nil, "general", nil, nil, "yo", json.RawMessage(`{}`), "delivered", time.Now()))

	rec := doPoll(t, mock, "agent=tester&after=5&timeout=1")

	resp := decodePollResponse(t, rec)
	if len(resp.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(resp.Messages))
	}
	if resp.LastID != 7 {
		t.Errorf("last_id = %d, want 7", resp.LastID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPollGarbageAfterIsRejected(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	rec := doPoll(t, mock, "agent=tester&after=abc&timeout=1")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

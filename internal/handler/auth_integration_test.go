package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
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
	mux.HandleFunc("GET /identity", Identity("off"))
	mux.Handle("/agents", Agents(pool))
	mux.Handle("/agents/", Agents(pool))
	mux.Handle("/messages", Messages(pool))
	mux.Handle("/messages/", Messages(pool))
	mux.Handle("/messages/poll", Poll(pool, NewNotifier()))
	mux.Handle("/messages/stream", Stream(pool))
	required := AuthMiddleware(pool, "required", admin, mux)
	optional := AuthMiddleware(pool, "optional", admin, mux)
	off := AuthMiddleware(pool, "off", admin, mux)
	do := func(handler http.Handler, method, path, key, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
		return rec
	}
	request := func(handler http.Handler, path, key, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		return do(handler, http.MethodPost, path, key, body, want)
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

	t.Run("private enrollment", func(t *testing.T) {
		private := PrivateEnrollment(pool)
		pair := func(name, key string) string {
			return fmt.Sprintf(`{"agent":%q,"key_hash":%q}`, name, hashKey(key))
		}
		claim := func(name, key string, want int) {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, "/enroll", strings.NewReader(pair(name, key))).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			private.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("claim %q returned %d, want %d: %s", name, rec.Code, want, rec.Body)
			}
			if rec.Code == http.StatusOK && rec.Body.String() != fmt.Sprintf("{\"agent\":%q}\n", name) {
				t.Fatalf("unexpected claim response: %s", rec.Body)
			}
		}
		const key = "new-agent-private-key"
		claim("system", key, http.StatusBadRequest)
		do(private, http.MethodGet, "/identity", key, "", http.StatusNotFound)
		do(required, http.MethodPost, "/enroll", admin, pair("private-agent", key), http.StatusNotFound)
		claim("private-agent", key, http.StatusOK)
		claim("private-agent", key, http.StatusOK)
		claim("private-agent", "different-key", http.StatusConflict)
		claim("different-agent", key, http.StatusConflict)
		do(required, http.MethodGet, "/identity", key, "", http.StatusOK)
		issueKey(required, "reserved-legacy-key")
		claim("reserved-legacy-key", "another-key", http.StatusConflict)
		request(required, "/agents", admin, `{"name":"unkeyed-agent","role":"worker"}`, http.StatusCreated)
		claim("unkeyed-agent", "another-key", http.StatusConflict)
		// A message to an unclaimed name must not block its first enrollment.
		if _, err := pool.Exec(ctx, `INSERT INTO messages (sender, "to", message) VALUES ('host-sender', 'future-recipient', 'start reviewing')`); err != nil {
			t.Fatal(err)
		}
		claim("future-recipient", "future-recipient-key", http.StatusOK)
		claim("host-sender", "another-key", http.StatusConflict)
		// Simulate an older database containing history from before the trigger.
		if _, err := pool.Exec(ctx, `
			ALTER TABLE messages DISABLE TRIGGER message_inserted;
			INSERT INTO messages (sender, "to", message) VALUES
				('former-sender', 'former-recipient', 'history'),
				('system', NULL, 'former-joined joined — worked on docs'),
				('system', NULL, 'former-left left');
			DELETE FROM murmur_schema_migrations WHERE name = 'reserve-message-identities'
		`); err != nil {
			t.Fatal(err)
		}
		if err := schema.Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE sender IN ('former-sender', 'system')`); err != nil {
			t.Fatal(err)
		}
		claim("former-sender", "another-key", http.StatusConflict)
		claim("former-joined", "another-key", http.StatusConflict)
		claim("former-left", "another-key", http.StatusConflict)
		claim("former-recipient", "former-recipient-key", http.StatusOK)
		// Keep the roster checks below limited to their own agents.
		do(required, http.MethodDelete, "/agents/future-recipient", admin, "", http.StatusOK)
		do(required, http.MethodDelete, "/agents/former-recipient", admin, "", http.StatusOK)
		claim("former-recipient", "former-recipient-key", http.StatusConflict)
		if err := schema.ReserveAgentNames(ctx, pool, []string{"former-memory-author"}); err != nil {
			t.Fatal(err)
		}
		claim("former-memory-author", "another-key", http.StatusConflict)
		do(required, http.MethodDelete, "/agents/private-agent", key, "", http.StatusOK)
		claim("private-agent", key, http.StatusConflict)
	})

	enroll := func(name string) string {
		t.Helper()
		rec := request(off, "/keys", admin, fmt.Sprintf(`{"agent":%q,"enroll":true}`, name), http.StatusCreated)
		var credential createKeyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &credential); err != nil || credential.Key == "" {
			t.Fatalf("invalid enrollment response: %v", err)
		}
		return credential.Key
	}
	enrolledKey := enroll("enrolled-first")
	var identity model.Identity
	rec := do(off, http.MethodGet, "/identity", enrolledKey, "", http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &identity); err != nil || identity.Agent != "enrolled-first" || identity.Admin || identity.ChatAuth != "off" {
		t.Fatalf("auth-off identity response: %s (%v)", rec.Body, err)
	}
	do(off, http.MethodGet, "/identity", "", "", http.StatusUnauthorized)
	request(required, "/keys", enrolledKey, `{"agent":"someone-else","enroll":true}`, http.StatusUnauthorized)
	request(required, "/agents/enrolled-first/heartbeat", enrolledKey, "", http.StatusOK)

	request(off, "/agents", admin, `{"name":"legacy-roster","role":"worker"}`, http.StatusCreated)
	for _, tc := range []struct {
		key        string
		showStatus bool
	}{
		{admin, true},
		{enrolledKey, false},
	} {
		roster := do(required, http.MethodGet, "/agents", tc.key, "", http.StatusOK)
		var agents []model.Agent
		if err := json.Unmarshal(roster.Body.Bytes(), &agents); err != nil || len(agents) < 2 {
			t.Fatalf("roster: %s (%v)", roster.Body, err)
		}
		for _, agent := range agents {
			if !tc.showStatus {
				if agent.KeyEnrolled != nil {
					t.Fatal("enrollment information exposed to an ordinary agent")
				}
				continue
			}
			if agent.KeyEnrolled == nil || *agent.KeyEnrolled != (agent.Name == "enrolled-first") {
				t.Fatalf("wrong enrollment label for %s", agent.Name)
			}
		}
	}

	agentKey := issueKey(required, "registers-first")
	request(required, "/agents", agentKey, `{"name":"registers-first","role":"worker"}`, http.StatusCreated)
	senderKey := issueKey(mux, "sends-first")
	request(required, "/messages", senderKey, `{"sender":"sends-first","message":"hello"}`, http.StatusCreated)
	request(required, "/keys", senderKey, `{"agent":"sends-first"}`, http.StatusUnauthorized)

	grant := request(required, "/agents", admin, `{"name":"registers-first","role":"lead","groups":["trusted"],"description":"admin assigned","capabilities":["review"]}`, http.StatusCreated)
	leadKey := enroll("registers-first")
	leadIdentity := do(off, http.MethodGet, "/identity", leadKey, "", http.StatusOK)
	if err := json.Unmarshal(leadIdentity.Body.Bytes(), &identity); err != nil || !identity.Lead || identity.Agent != "registers-first" {
		t.Fatalf("enrollment altered the lead grant: %s (%v)", leadIdentity.Body, err)
	}
	rejoin := request(required, "/agents", agentKey, `{"name":"registers-first","role":"lead","groups":["other"],"description":"changed","capabilities":["deploy"]}`, http.StatusCreated)
	var original, current model.Agent
	if err := json.Unmarshal(grant.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rejoin.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.SessionID != original.SessionID || !slices.Equal(current.Groups, original.Groups) ||
		!slices.Equal(current.Capabilities, original.Capabilities) || current.Description == nil ||
		*current.Description != "admin assigned" {
		t.Fatalf("non-admin rejoin altered protected lead fields: %+v", current)
	}
	request(optional, "/agents", "", `{"name":"registers-first","role":"lead","groups":["other"]}`, http.StatusForbidden)
	request(off, "/agents", "", `{"name":"registers-first","role":"lead","groups":["other"]}`, http.StatusForbidden)
	request(required, "/agents", agentKey, `{"name":"registers-first","role":"worker"}`, http.StatusForbidden)
	request(mux, "/agents", "", `{"name":"registers-first","role":"worker"}`, http.StatusForbidden)
	do(required, http.MethodDelete, "/agents/registers-first", agentKey, "", http.StatusForbidden)
	do(optional, http.MethodDelete, "/agents/registers-first", "", "", http.StatusForbidden)
	do(off, http.MethodDelete, "/agents/registers-first", "", "", http.StatusForbidden)
	request(required, "/agents/registers-first/heartbeat", agentKey, "", http.StatusOK)
	request(off, "/agents", admin, `{"name":"registers-first","role":"worker"}`, http.StatusCreated)
	request(off, "/agents", admin, `{"name":"registers-first","role":"lead"}`, http.StatusCreated)
	do(off, http.MethodDelete, "/agents/registers-first", admin, "", http.StatusOK)
	request(required, "/agents/registers-first/heartbeat", agentKey, "", http.StatusUnauthorized)

	freshKey := issueKey(required, "registers-first")
	request(required, "/agents", freshKey, `{"name":"registers-first","role":"worker","groups":["team"]}`, http.StatusCreated)
	outsiderKey := issueKey(required, "others")
	request(required, "/agents", outsiderKey, `{"name":"others","role":"worker"}`, http.StatusCreated)

	createMessage := func(body string) int {
		t.Helper()
		rec := request(required, "/messages", senderKey, body, http.StatusCreated)
		var result postMessageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.ID <= 0 {
			t.Fatalf("invalid message response: %v", err)
		}
		return result.ID
	}
	directID := createMessage(`{"sender":"sends-first","to":"registers-first","message":"direct"}`)
	direct := fmt.Sprintf("/messages/%d", directID)
	do(required, http.MethodGet, direct, outsiderKey, "", http.StatusNotFound)
	do(required, http.MethodGet, direct, freshKey, "", http.StatusOK)
	do(required, http.MethodGet, direct, senderKey, "", http.StatusOK)
	do(required, http.MethodGet, direct, admin, "", http.StatusOK)
	do(optional, http.MethodGet, direct, "", "", http.StatusOK)

	list := do(required, http.MethodGet, "/messages", outsiderKey, "", http.StatusOK)
	var history listMessagesResponse
	if err := json.Unmarshal(list.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	for _, msg := range history.Messages {
		if msg.ID == directID {
			t.Fatal("direct message appeared in an unrelated agent's history")
		}
	}
	request(required, direct+"/ack", outsiderKey, `{"agent":"others"}`, http.StatusNotFound)
	request(required, direct+"/ack", senderKey, `{"agent":"sends-first"}`, http.StatusNotFound)

	presence := request(required, "/agents/registers-first/heartbeat?inbox=false", freshKey, "", http.StatusOK)
	var heartbeat heartbeatResponse
	if err := json.Unmarshal(presence.Body.Bytes(), &heartbeat); err != nil || heartbeat.Inbox != nil || heartbeat.Name != "registers-first" {
		t.Fatalf("presence heartbeat must omit the inbox: %s (%v)", presence.Body, err)
	}
	senderPollPath := fmt.Sprintf("/messages/poll?agent=sends-first&after=%d", directID-1)
	for _, tc := range []struct {
		mode    string
		handler http.Handler
		key     string
	}{
		{"off", off, ""},
		{"optional", optional, ""},
		{"required", required, senderKey},
	} {
		senderPoll := do(tc.handler, http.MethodGet, senderPollPath, tc.key, "", http.StatusOK)
		var sent pollResponse
		if err := json.Unmarshal(senderPoll.Body.Bytes(), &sent); err != nil || len(sent.Messages) == 0 ||
			sent.Messages[0].ID != directID || sent.Messages[0].Status != "sent" {
			t.Fatalf("%s sender poll should leave outgoing status sent: %s (%v)", tc.mode, senderPoll.Body, err)
		}
	}
	pollPath := fmt.Sprintf("/messages/poll?agent=registers-first&after=%d", directID-1)
	poll := do(required, http.MethodGet, pollPath, freshKey, "", http.StatusOK)
	var inbox pollResponse
	if err := json.Unmarshal(poll.Body.Bytes(), &inbox); err != nil || len(inbox.Messages) == 0 || inbox.Messages[0].ID != directID || inbox.Messages[0].Status != "delivered" {
		t.Fatalf("direct message missing from recipient poll: %v", err)
	}
	request(required, direct+"/ack", freshKey, `{"agent":"registers-first"}`, http.StatusOK)
	groupID := createMessage(`{"sender":"sends-first","to":"@team","message":"group"}`)
	group := fmt.Sprintf("/messages/%d", groupID)
	do(required, http.MethodGet, group, outsiderKey, "", http.StatusNotFound)
	request(required, group+"/ack", freshKey, `{"agent":"registers-first"}`, http.StatusOK)
	request(required, group+"/ack", admin, `{"agent":"operator"}`, http.StatusOK)

	// Omitting ?agent must still filter the stream by its authenticated key.
	server := httptest.NewServer(required)
	defer server.Close()
	stream, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/messages/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.Header.Set("Authorization", "Bearer "+outsiderKey)
	response, err := server.Client().Do(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", response.StatusCode)
	}
	createMessage(`{"sender":"sends-first","to":"registers-first","message":"hidden from stream"}`)
	broadcast := createMessage(`{"sender":"sends-first","message":"broadcast"}`)
	readStreamMessage := func(body io.Reader) model.Message {
		t.Helper()
		reader := bufio.NewReader(body)
		event, err := reader.ReadString('\n')
		if err != nil || event != "event: message\n" {
			t.Fatalf("stream event %q: %v", event, err)
		}
		data, err := reader.ReadString('\n')
		value, ok := strings.CutPrefix(data, "data: ")
		var received model.Message
		if err != nil || !ok || json.Unmarshal([]byte(value), &received) != nil {
			t.Fatalf("invalid stream message %q: %v", data, err)
		}
		return received
	}
	if received := readStreamMessage(response.Body); received.ID != broadcast {
		t.Fatalf("stream exposed an unrelated direct message: %+v", received)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	// A named auth-off stream uses the requested agent for receipt tracking.
	// An unaddressed outgoing message can be visible without being delivered.
	offServer := httptest.NewServer(off)
	defer offServer.Close()
	stream, err = http.NewRequestWithContext(ctx, http.MethodGet, offServer.URL+"/messages/stream?agent=sends-first", nil)
	if err != nil {
		t.Fatal(err)
	}
	ownStream, err := offServer.Client().Do(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer ownStream.Body.Close()
	if ownStream.StatusCode != http.StatusOK {
		t.Fatalf("auth-off stream status %d", ownStream.StatusCode)
	}
	privateID := createMessage(`{"sender":"sends-first","to":"registers-first","message":"private outgoing"}`)
	broadcastID := createMessage(`{"sender":"sends-first","message":"own broadcast"}`)
	received := readStreamMessage(ownStream.Body)
	if received.ID != broadcastID || received.Status != "sent" {
		t.Fatalf("sender stream should skip the direct message and leave its broadcast sent: %+v", received)
	}
	newest := do(required, http.MethodGet, "/messages?channel=&order=desc&limit=2", admin, "", http.StatusOK)
	var latest listMessagesResponse
	if err := json.Unmarshal(newest.Body.Bytes(), &latest); err != nil || len(latest.Messages) != 2 ||
		latest.Messages[0].ID != broadcastID || latest.Messages[1].ID != privateID {
		t.Fatalf("dashboard recent messages: %s (%v)", newest.Body, err)
	}

	for _, id := range []int{privateID, broadcastID} {
		record := do(required, http.MethodGet, fmt.Sprintf("/messages/%d", id), admin, "", http.StatusOK)
		var unchanged model.Message
		if err := json.Unmarshal(record.Body.Bytes(), &unchanged); err != nil || unchanged.Status != "sent" {
			t.Fatalf("sender stream marked message %d delivered: %s (%v)", id, record.Body, err)
		}
	}
}

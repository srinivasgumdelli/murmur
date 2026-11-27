package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type registerAgentRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Description  *string  `json:"description"`
	Capabilities []string `json:"capabilities"`
	Groups       []string `json:"groups"`
	// ConfirmSelfEnrolled lets an admin register or promote a self-enrolled
	// agent after checking who holds its key. It clears the self-enrolled mark.
	ConfirmSelfEnrolled bool `json:"confirm_self_enrolled,omitempty"`
}

func Agents(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/agents")
		path = strings.TrimPrefix(path, "/")

		switch {
		case path == "" && r.Method == http.MethodPost:
			registerAgent(pool, w, r)
		case path == "" && r.Method == http.MethodGet:
			listAgents(pool, w, r)
		case strings.HasSuffix(path, "/heartbeat") && r.Method == http.MethodPost:
			agentName := strings.TrimSuffix(path, "/heartbeat")
			heartbeat(pool, w, r, agentName)
		case path != "" && !strings.Contains(path, "/") && r.Method == http.MethodDelete:
			deleteAgent(pool, w, r, path)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

func registerAgent(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
	var req registerAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Role == "" {
		http.Error(w, "name and role are required", http.StatusBadRequest)
		return
	}
	if !checkAgent(w, r, req.Name) {
		return
	}
	actor := principalFromContext(r.Context())
	if req.Capabilities == nil {
		req.Capabilities = []string{}
	}
	if req.Groups == nil {
		req.Groups = []string{}
	}

	var a model.Agent
	// Hold the per-name lock that private claims take, so a claim for a
	// case variant cannot commit alongside this registration.
	err := pgx.BeginFunc(r.Context(), pool, func(tx pgx.Tx) error {
		if err := lockAgentName(r.Context(), tx, req.Name); err != nil {
			return err
		}
		// A request queued behind a reclaim must not restore the claimant's
		// metadata with a key that the reclaim just revoked.
		if revoked, err := keyRevoked(r.Context(), tx, actor); err != nil {
			return err
		} else if revoked {
			return errKeyRevoked
		}
		if actor.admin {
			if err := checkSelfEnrolledVariant(r.Context(), tx, req.Name); err != nil {
				return err
			}
		}
		return tx.QueryRow(r.Context(),
			`INSERT INTO agents (name, session_id, role, description, capabilities, groups, status, last_seen, memory_lead)
		 VALUES ($1, gen_random_uuid()::text, $2, $3, $4, $5, 'online', now(), $6 AND $2 = 'lead')
		 ON CONFLICT (name) DO UPDATE SET
		 session_id = CASE WHEN $6 OR NOT agents.memory_lead THEN gen_random_uuid()::text ELSE agents.session_id END,
		 role = $2,
		 memory_lead = CASE WHEN $6 THEN $2 = 'lead' ELSE agents.memory_lead END,
		 description = CASE WHEN $6 OR NOT agents.memory_lead THEN $3 ELSE agents.description END,
		 capabilities = CASE WHEN $6 OR NOT agents.memory_lead THEN $4 ELSE agents.capabilities END,
		 groups = CASE WHEN $6 OR NOT agents.memory_lead THEN $5 ELSE agents.groups END,
		 self_enrolled = CASE WHEN $6 AND $8 THEN false ELSE agents.self_enrolled END,
		 status = 'online', last_seen = now()
		 WHERE ($6 OR NOT agents.memory_lead OR ($7 AND agents.role = $2))
		   AND NOT ($6 AND agents.self_enrolled AND NOT $8)
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
			req.Name, req.Role, req.Description, req.Capabilities, req.Groups, actor.admin, actor.agent == req.Name, req.ConfirmSelfEnrolled,
		).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	})
	if errors.Is(err, errKeyRevoked) {
		http.Error(w, "unauthorized: valid Murmur key required", http.StatusUnauthorized)
		return
	}
	if errors.Is(err, errSelfEnrolledVariant) {
		http.Error(w, "a self-enrolled agent already uses "+req.Name+" in another letter case; reclaim or delete that agent first", http.StatusConflict)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) && actor.admin {
		// The self-enrolled guard is the only way an admin upsert matches no
		// row. Registering over a claim would otherwise look like reserving
		// the name while the claimant keeps its key.
		http.Error(w, "agent "+req.Name+" claimed its name through private enrollment; check who holds its key, then send confirm_self_enrolled:true, or reclaim it with POST /keys", http.StatusConflict)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "forbidden: a memory lead can rejoin with its own key and assigned role, or be registered by an admin", http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("upsert agent: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	joinMsg := a.Name + " joined"
	if a.Description != nil && *a.Description != "" {
		joinMsg += " — " + *a.Description
	} else {
		joinMsg += " (" + a.Role + ")"
	}
	postSystemMessage(r.Context(), pool, joinMsg)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

func listAgents(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
	rows, err := pool.Query(r.Context(),
		`SELECT name, session_id, role, description, capabilities, groups, status, last_seen, memory_lead,
		    CASE WHEN $1 THEN EXISTS (SELECT 1 FROM api_keys WHERE agent = agents.name) END,
		    CASE WHEN $1 THEN self_enrolled END
		 FROM agents ORDER BY last_seen DESC`, principalFromContext(r.Context()).admin)
	if err != nil {
		log.Printf("query agents: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen, &a.MemoryLead, &a.KeyEnrolled, &a.SelfEnrolled); err != nil {
			log.Printf("scan agent: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		log.Printf("list agents: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if agents == nil {
		agents = []model.Agent{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agents)
}

func deleteAgent(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, name string) {
	if !checkAgent(w, r, name) {
		return
	}
	tx, err := pool.Begin(r.Context())
	if err != nil {
		log.Printf("begin tx: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	// Take the per-name lock that reclaim holds, then recheck the caller's
	// key, so a delete queued behind a reclaim cannot remove the operator's
	// new key.
	if err := lockAgentName(r.Context(), tx, name); err != nil {
		log.Printf("lock agent %s for deletion: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if revoked, err := keyRevoked(r.Context(), tx, principalFromContext(r.Context())); err != nil {
		log.Printf("recheck key for deletion of %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if revoked {
		http.Error(w, "unauthorized: valid Murmur key required", http.StatusUnauthorized)
		return
	}

	// Lock the row before reading the grant or deleting keys. A concurrent
	// role change and deletion must agree which one happened first.
	var lead bool
	err = tx.QueryRow(r.Context(), `SELECT memory_lead FROM agents WHERE name = $1 FOR UPDATE`, name).Scan(&lead)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("look up agent %s for deletion: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if lead && !principalFromContext(r.Context()).admin {
		http.Error(w, "forbidden: only an admin can delete a memory lead", http.StatusForbidden)
		return
	}

	if _, err := tx.Exec(r.Context(), `INSERT INTO reserved_agent_names (name) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
		log.Printf("reserve deleted agent name %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, err := tx.Exec(r.Context(), `DELETE FROM api_keys WHERE agent = $1`, name); err != nil {
		log.Printf("delete api_keys for agent %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var a model.Agent
	err = tx.QueryRow(r.Context(),
		`DELETE FROM agents WHERE name = $1
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
		name,
	).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	if err != nil {
		log.Printf("delete agent %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("commit delete agent %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	postSystemMessage(r.Context(), pool, a.Name+" left")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

type heartbeatResponse struct {
	model.Agent
	Inbox *Inbox `json:"inbox,omitempty"`
}

func heartbeat(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, name string) {
	if !checkAgent(w, r, name) {
		return
	}
	var a model.Agent
	err := pool.QueryRow(r.Context(),
		`UPDATE agents SET status = 'online', last_seen = now()
		 WHERE name = $1
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
		name,
	).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	out := heartbeatResponse{Agent: a}
	if r.URL.Query().Get("inbox") != "false" {
		out.Inbox = fetchInbox(r.Context(), pool, name)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

package handler

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

var errEnrollmentConflict = errors.New("agent name or credential already reserved")

// PrivateEnrollment serves only the unpublished Docker listener.
func PrivateEnrollment(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return
		}

		var req struct {
			Agent   string `json:"agent"`
			KeyHash string `json:"key_hash"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(new(any)) != io.EOF || !model.ValidAgentName(req.Agent) || req.Agent == "system" {
			http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		hash, err := hex.DecodeString(req.KeyHash)
		if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != req.KeyHash {
			http.Error(w, "key_hash must be a lowercase SHA-256 digest", http.StatusBadRequest)
			return
		}

		switch err := claimAgent(r.Context(), pool, req.Agent, req.KeyHash); {
		case errors.Is(err, errEnrollmentConflict):
			http.Error(w, errEnrollmentConflict.Error(), http.StatusConflict)
		case err != nil:
			log.Printf("private enrollment: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				Agent string `json:"agent"`
			}{Agent: req.Agent})
		}
	})
	return mux
}

func claimAgent(ctx context.Context, pool *pgxpool.Pool, agent, keyHash string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inserted, err := tx.Exec(ctx, `INSERT INTO agents (name, session_id, role, status)
			VALUES ($1, gen_random_uuid()::text, 'coding-agent', 'offline')
			ON CONFLICT (name) DO NOTHING`, agent)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 0 {
			// A retry can confirm its own prior claim. No key is returned.
			var sameClaim bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM api_keys AS k JOIN agents AS a ON a.name = k.agent
				WHERE k.agent = $1 AND k.key_hash = $2
			)`, agent, keyHash).Scan(&sameClaim)
			if err != nil {
				return err
			}
			if !sameClaim {
				return errEnrollmentConflict
			}
			return nil
		}
		// Check after inserting the agent row, so a delete that just freed the
		// name can no longer race with this check.
		var reserved bool
		err = tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM reserved_agent_names WHERE name = $1)
			OR EXISTS (SELECT 1 FROM api_keys WHERE agent = $1)`, agent).Scan(&reserved)
		if err != nil {
			return err
		}
		if reserved {
			return errEnrollmentConflict
		}

		key, err := tx.Exec(ctx, `INSERT INTO api_keys (key_hash, agent) VALUES ($1, $2)
			ON CONFLICT (key_hash) DO NOTHING`, keyHash, agent)
		if err == nil && key.RowsAffected() == 0 {
			return errEnrollmentConflict
		}
		return err
	})
}

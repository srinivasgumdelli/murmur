package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type createKeyRequest struct {
	Agent  string `json:"agent"`
	Enroll bool   `json:"enroll,omitempty"`
	// Reclaim revokes the keys of a self-enrolled agent before issuing one,
	// for an operator taking back a name claimed on the private listener.
	Reclaim bool `json:"reclaim,omitempty"`
}

var errSelfEnrolled = errors.New("agent claimed its name through private enrollment")

type createKeyResponse struct {
	Key   string `json:"key"`
	Agent string `json:"agent"`
}

func generateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("mmr_%s", hex.EncodeToString(b)), nil
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func Keys(pool *pgxpool.Pool, adminKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminKey == "" {
			http.Error(w, "key management disabled (MURMUR_ADMIN_KEY not set)", http.StatusNotFound)
			return
		}
		if !matchesAdminKey(requestKey(r), adminKey) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req createKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Agent == "" {
			http.Error(w, "agent is required", http.StatusBadRequest)
			return
		}

		if req.Enroll && !model.ValidAgentName(req.Agent) {
			http.Error(w, "invalid agent name", http.StatusBadRequest)
			return
		}

		key, err := generateKey()
		if err != nil {
			log.Printf("generate key: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		err = issueKey(r.Context(), pool, req, hashKey(key))
		if errors.Is(err, errSelfEnrolledVariant) {
			http.Error(w, "a self-enrolled agent already uses "+req.Agent+" in another letter case; reclaim or delete that agent first", http.StatusConflict)
			return
		}
		if errors.Is(err, errSelfEnrolled) {
			http.Error(w, "agent "+req.Agent+" claimed its name through private enrollment; send reclaim:true to revoke its keys and issue a new one", http.StatusConflict)
			return
		}
		if err != nil {
			log.Printf("insert key: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(createKeyResponse{Key: key, Agent: req.Agent})
	}
}

func issueKey(ctx context.Context, pool *pgxpool.Pool, req createKeyRequest, keyHash string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Wait for any in-flight private claim of this name to commit, so the
		// self-enrolled check below sees it.
		if err := lockAgentName(ctx, tx, req.Agent); err != nil {
			return err
		}
		if err := checkSelfEnrolledVariant(ctx, tx, req.Agent); err != nil {
			return err
		}
		if req.Enroll {
			// An enrolled identity keeps its existing role, groups and session.
			if _, err := tx.Exec(ctx, `INSERT INTO agents (name, session_id, role, status)
				VALUES ($1, gen_random_uuid()::text, 'coding-agent', 'offline')
				ON CONFLICT (name) DO NOTHING`, req.Agent); err != nil {
				return err
			}
		}
		// A self-enrolled name may belong to whoever reached the private
		// listener first. Never add a second key to it silently.
		var selfEnrolled bool
		err := tx.QueryRow(ctx, `SELECT self_enrolled FROM agents WHERE name = $1 FOR UPDATE`, req.Agent).Scan(&selfEnrolled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if selfEnrolled {
			if !req.Reclaim {
				return errSelfEnrolled
			}
			if _, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE agent = $1`, req.Agent); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE agents SET self_enrolled = false, role = 'coding-agent', description = NULL,
				capabilities = '{}', groups = '{}', memory_lead = false WHERE name = $1`, req.Agent); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO api_keys (key_hash, agent) VALUES ($1, $2)`, keyHash, req.Agent)
		return err
	})
}

func Identity(authMode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFromContext(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(model.Identity{Agent: p.agent, Admin: p.admin, Lead: p.lead, ChatAuth: authMode})
	}
}

type principal struct {
	agent string
	admin bool
	lead  bool
	// keyHash identifies the agent key for long-lived requests, which
	// recheck it so a revoked key (reclaim or delete) loses access.
	keyHash string
}

var errKeyRevoked = errors.New("agent key was revoked")

// keyRevoked reports whether the request's agent key no longer exists.
// Admin and unauthenticated principals carry no key hash and never count as
// revoked. A lookup error is returned separately, so callers can treat a
// transient database failure as an error rather than as a logout.
func keyRevoked(ctx context.Context, pool keyLookup, p principal) (bool, error) {
	if p.keyHash == "" || pool == nil {
		return false, nil
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE key_hash = $1)`, p.keyHash).Scan(&valid); err != nil {
		return false, err
	}
	return !valid, nil
}

type principalKey struct{}

func principalFromContext(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

func AgentFromContext(ctx context.Context) string {
	return principalFromContext(ctx).agent
}

// requestKey supports key management even when chat authentication is off.
func requestKey(r *http.Request) string {
	if key := r.Header.Get("X-Murmur-Key"); key != "" {
		return key
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

func matchesAdminKey(provided, adminKey string) bool {
	return adminKey != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(adminKey)) == 1
}

func checkAgent(w http.ResponseWriter, r *http.Request, agent string) bool {
	p := principalFromContext(r.Context())
	if p.admin || p.agent == "" || p.agent == agent {
		return true
	}
	http.Error(w, "forbidden: key belongs to a different agent", http.StatusForbidden)
	return false
}

type keyLookup interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func AuthMiddleware(pool keyLookup, authMode string, adminKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/", "/connect", "/dashboard.css", "/dashboard.js":
			next.ServeHTTP(w, r)
			return
		}

		key := requestKey(r)
		identityRoute := r.URL.Path == "/identity"
		if !identityRoute && (authMode == "" || authMode == "off") {
			// Chat remains open. The configured admin key can still manage
			// protected roles when the server is running without chat auth.
			if matchesAdminKey(key, adminKey) {
				r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{admin: true}))
			}
			next.ServeHTTP(w, r)
			return
		}

		var p principal
		switch {
		case matchesAdminKey(key, adminKey):
			p.admin = true
		case key != "":
			err := pool.QueryRow(r.Context(),
				`SELECT k.agent, COALESCE(a.memory_lead AND a.role = 'lead', false)
				 FROM api_keys k LEFT JOIN agents a ON a.name = k.agent WHERE k.key_hash = $1`,
				hashKey(key),
			).Scan(&p.agent, &p.lead)
			if errors.Is(err, pgx.ErrNoRows) {
				p = principal{}
			} else if err == nil {
				p.keyHash = hashKey(key)
			} else {
				log.Printf("auth: look up key: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		if p.agent == "" && !p.admin {
			if authMode != "optional" || identityRoute {
				http.Error(w, "unauthorized: valid Murmur key required; GET /connect for setup instructions", http.StatusUnauthorized)
				return
			}
			log.Printf("auth: unauthenticated request to %s", r.URL.Path)
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

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
)

type createKeyRequest struct {
	Agent string `json:"agent"`
}

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

		key, err := generateKey()
		if err != nil {
			log.Printf("generate key: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		_, err = pool.Exec(r.Context(),
			`INSERT INTO api_keys (key_hash, agent) VALUES ($1, $2)`,
			hashKey(key), req.Agent,
		)
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

type principal struct {
	agent string
	admin bool
	lead  bool
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
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		key := requestKey(r)
		if authMode == "" || authMode == "off" {
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
			} else if err != nil {
				log.Printf("auth: look up key: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		if p.agent == "" && !p.admin {
			if authMode != "optional" {
				http.Error(w, "unauthorized: valid Murmur key required", http.StatusUnauthorized)
				return
			}
			log.Printf("auth: unauthenticated request to %s", r.URL.Path)
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

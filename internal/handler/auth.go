package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

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
		provided := r.Header.Get("X-Murmur-Key")
		if provided != adminKey {
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

func AuthMiddleware(pool *pgxpool.Pool, authMode string, adminKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authMode == "" || authMode == "off" {
			next.ServeHTTP(w, r)
			return
		}

		// Skip auth for health and dashboard
		if r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("X-Murmur-Key")
		if key == "" {
			if authMode == "optional" {
				log.Printf("auth: unauthenticated request to %s", r.URL.Path)
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized: X-Murmur-Key header required", http.StatusUnauthorized)
			return
		}

		// Admin key bypasses agent check
		if key == adminKey {
			next.ServeHTTP(w, r)
			return
		}

		var agent string
		err := pool.QueryRow(r.Context(),
			`SELECT agent FROM api_keys WHERE key_hash = $1`,
			hashKey(key),
		).Scan(&agent)
		if err != nil {
			if authMode == "optional" {
				log.Printf("auth: invalid key for %s", r.URL.Path)
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized: invalid key", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

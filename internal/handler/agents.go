package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type registerAgentRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
}

func Agents(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			registerAgent(pool, w, r)
		case http.MethodGet:
			listAgents(pool, w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	if req.Capabilities == nil {
		req.Capabilities = []string{}
	}

	var a model.Agent
	err := pool.QueryRow(r.Context(),
		`INSERT INTO agents (name, role, capabilities, last_seen)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (name) DO UPDATE SET role = $2, capabilities = $3, last_seen = now()
		 RETURNING name, role, capabilities, last_seen`,
		req.Name, req.Role, req.Capabilities,
	).Scan(&a.Name, &a.Role, &a.Capabilities, &a.LastSeen)
	if err != nil {
		log.Printf("upsert agent: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

func listAgents(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
	rows, err := pool.Query(r.Context(),
		`SELECT name, role, capabilities, last_seen FROM agents ORDER BY last_seen DESC`)
	if err != nil {
		log.Printf("query agents: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(&a.Name, &a.Role, &a.Capabilities, &a.LastSeen); err != nil {
			log.Printf("scan agent: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		agents = append(agents, a)
	}
	if agents == nil {
		agents = []model.Agent{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agents)
}

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/handler"
	"github.com/srinivasgumdelli/murmur/internal/schema"
	"github.com/srinivasgumdelli/murmur/internal/ui"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	dbURL := getenv("BUS_DATABASE_URL", "postgres://murmur:murmur@localhost:5432/murmur?sslmode=disable")
	port := getenv("BUS_PORT", "4444")

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	if err := schema.Apply(ctx, pool); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	log.Printf("schema applied")

	startTime := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/messages", handler.Messages(pool))
	mux.HandleFunc("/messages/stream", handler.Stream(pool))
	mux.HandleFunc("/agents", handler.Agents(pool))
	mux.HandleFunc("/health", handler.Health(pool, startTime))
	mux.HandleFunc("/", ui.Handler())

	log.Printf("murmur ready on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

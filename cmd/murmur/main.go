package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/handler"
	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/memory/pgstore"
	"github.com/srinivasgumdelli/murmur/internal/memory/sqlitestore"
	"github.com/srinivasgumdelli/murmur/internal/schema"
	"github.com/srinivasgumdelli/murmur/internal/ui"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func positiveSetting(key string, fallback, max int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		log.Fatalf("%s must be an integer between 1 and %d", key, max)
	}
	return value
}

func main() {
	dbURL := getenv("BUS_DATABASE_URL", "postgres://murmur:murmur@localhost:5432/murmur?sslmode=disable")
	port := getenv("BUS_PORT", "4444")
	adminKey := getenv("MURMUR_ADMIN_KEY", "")
	authMode := getenv("MURMUR_AUTH", "off")
	messageTTL := getenv("MURMUR_MESSAGE_TTL", "")

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

	notifier := handler.NewNotifier()
	notifier.Listen(ctx, pool)

	var memoryStore memory.Store
	switch backend := getenv("MURMUR_MEMORY_STORE", "postgres"); backend {
	case "postgres":
		store := pgstore.New(pool)
		if err := store.Apply(ctx); err != nil {
			log.Fatalf("apply Postgres memory schema: %v", err)
		}
		memoryStore = store
	case "sqlite":
		store, err := sqlitestore.Open(ctx, getenv("MURMUR_MEMORY_DB", "./murmur-memory.db"))
		if err != nil {
			log.Fatalf("open SQLite memory store: %v", err)
		}
		defer store.Close()
		memoryStore = store
	default:
		log.Fatalf("unsupported MURMUR_MEMORY_STORE %q", backend)
	}
	memoryService := memory.NewService(memoryStore, memory.Config{
		AuthMode: authMode,
		TTLDays:  positiveSetting("MURMUR_MEMORY_TTL_DAYS", 90, 36500),
		ScopeCap: positiveSetting("MURMUR_MEMORY_SCOPE_CAP", 1000, 1<<31-1),
		Publish:  notifier.PublishMemory,
	})
	memoryRetention := time.Duration(positiveSetting("MURMUR_MEMORY_KEEP_EXPIRED_DAYS", 30, 36500)) * 24 * time.Hour

	startTime := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/messages/stream", handler.MemoryStream(notifier, authMode, handler.Stream(pool)))
	mux.HandleFunc("/messages/poll", handler.Poll(pool, notifier))
	mux.HandleFunc("/messages/", handler.Messages(pool))
	mux.HandleFunc("/messages", handler.Messages(pool))
	mux.HandleFunc("/agents/", handler.Agents(pool))
	mux.HandleFunc("/agents", handler.Agents(pool))
	mux.HandleFunc("/keys", handler.Keys(pool, adminKey))
	mux.HandleFunc("/health", handler.Health(pool, startTime, memoryService.Health))
	handler.InstallMemoryRoutes(mux, memoryService, authMode)
	mux.Handle("/mcp", handler.MemoryMCP(memoryService, authMode))
	mux.HandleFunc("/", ui.Handler())

	var srv http.Handler = mux
	if authMode != "" && authMode != "off" {
		srv = handler.AuthMiddleware(pool, authMode, adminKey, mux)
		log.Printf("auth mode: %s", authMode)
	}

	handler.StartReaper(ctx, pool, messageTTL, func(ctx context.Context) error {
		return memoryService.Reap(ctx, memoryRetention)
	})
	log.Printf("murmur ready on :%s", port)
	if err := http.ListenAndServe(":"+port, srv); err != nil {
		log.Fatalf("server: %v", err)
	}
}

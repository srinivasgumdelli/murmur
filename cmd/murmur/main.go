package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/handler"
	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/schema"
	"github.com/srinivasgumdelli/murmur/internal/ui"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func positiveSetting(key string, fallback, max int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", key, max)
	}
	return value, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dbURL := getenv("BUS_DATABASE_URL", "postgres://murmur:murmur@localhost:5432/murmur?sslmode=disable")
	port := getenv("BUS_PORT", "4444")
	adminKey := getenv("MURMUR_ADMIN_KEY", "")
	authMode := getenv("MURMUR_AUTH", "off")
	messageTTL := getenv("MURMUR_MESSAGE_TTL", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer func() {
		cancel()
		pool.Close()
	}()

	if err := schema.Apply(ctx, pool); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	log.Printf("schema applied")

	notifier := handler.NewNotifier()
	notifier.Listen(ctx, pool)

	var memoryService *memory.Service
	var memoryCheck func(context.Context) error
	var reapMemory func(context.Context) error
	if authMode == "required" {
		ttlDays, err := positiveSetting("MURMUR_MEMORY_TTL_DAYS", 90, 36500)
		if err != nil {
			return err
		}
		scopeCap, err := positiveSetting("MURMUR_MEMORY_SCOPE_CAP", 1000, 1<<31-1)
		if err != nil {
			return err
		}
		keepExpiredDays, err := positiveSetting("MURMUR_MEMORY_KEEP_EXPIRED_DAYS", 30, 36500)
		if err != nil {
			return err
		}

		store, closeStore, err := openMemoryStore(ctx, pool)
		if err != nil {
			return err
		}
		if closeStore != nil {
			defer func() {
				cancel()
				closeStore()
			}()
		}
		memoryService = memory.NewService(store, memory.Config{
			AuthMode: authMode,
			TTLDays:  ttlDays,
			ScopeCap: scopeCap,
			Publish:  notifier.PublishMemory,
		})
		memoryCheck = memoryService.Health
		reapMemory = func(ctx context.Context) error {
			return memoryService.Reap(ctx, time.Duration(keepExpiredDays)*24*time.Hour)
		}
	}

	startTime := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/messages/stream", handler.MemoryStream(notifier, authMode, handler.Stream(pool)))
	mux.HandleFunc("/messages/poll", handler.Poll(pool, notifier))
	mux.HandleFunc("/messages/", handler.Messages(pool))
	mux.HandleFunc("/messages", handler.Messages(pool))
	mux.HandleFunc("/agents/", handler.Agents(pool))
	mux.HandleFunc("/agents", handler.Agents(pool))
	mux.HandleFunc("/keys", handler.Keys(pool, adminKey))
	mux.HandleFunc("/health", handler.Health(pool, startTime, memoryCheck))
	handler.InstallMemoryRoutes(mux, memoryService, authMode)
	mux.Handle("/mcp", handler.MemoryMCP(memoryService, authMode))
	mux.HandleFunc("/", ui.Handler())

	var srv http.Handler = mux
	if authMode != "" && authMode != "off" {
		srv = handler.AuthMiddleware(pool, authMode, adminKey, mux)
		log.Printf("auth mode: %s", authMode)
	}

	handler.StartReaper(ctx, pool, messageTTL, reapMemory)
	log.Printf("murmur ready on :%s", port)
	return http.ListenAndServe(":"+port, srv)
}

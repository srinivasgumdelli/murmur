package handler

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

func postSystemMessage(ctx context.Context, pool *pgxpool.Pool, message string) {
	_, err := pool.Exec(ctx,
		`INSERT INTO messages (sender, channel, message, metadata, status)
		 VALUES ('system', 'general', $1, '{}', 'sent')`,
		message,
	)
	if err != nil {
		log.Printf("system message: %v", err)
	}
}

package handler

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type Inbox struct {
	Messages []model.Message `json:"messages"`
	Count    int             `json:"count"`
}

func fetchInbox(ctx context.Context, pool *pgxpool.Pool, agent string) *Inbox {
	if agent == "" {
		return nil
	}

	rows, err := pool.Query(ctx,
		`SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at
		 FROM messages
		 WHERE (("to" = $1) OR ("to" IN (
		     SELECT '@' || unnest(groups) FROM agents WHERE name = $1
		 )))
		 AND status = 'sent'
		 ORDER BY id ASC
		 LIMIT 50`,
		agent,
	)
	if err != nil {
		log.Printf("inbox query: %v", err)
		return nil
	}
	defer rows.Close()

	var msgs []model.Message
	var ids []int
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.Sender, &m.SessionID, &m.Channel, &m.To, &m.ReplyTo, &m.Message, &m.Metadata, &m.Status, &m.CreatedAt); err != nil {
			log.Printf("inbox scan: %v", err)
			continue
		}
		msgs = append(msgs, m)
		ids = append(ids, m.ID)
	}

	if len(ids) > 0 {
		_, err := pool.Exec(ctx,
			`UPDATE messages SET status = 'delivered' WHERE id = ANY($1::int[])`,
			ids,
		)
		if err != nil {
			log.Printf("inbox mark delivered: %v", err)
		}
		for i := range msgs {
			msgs[i].Status = "delivered"
		}
	}

	if msgs == nil {
		msgs = []model.Message{}
	}

	return &Inbox{Messages: msgs, Count: len(msgs)}
}

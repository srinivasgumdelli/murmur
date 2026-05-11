package handler

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Notifier struct {
	mu      sync.Mutex
	waiters map[chan struct{}]string // chan -> agent name
}

func NewNotifier() *Notifier {
	return &Notifier{
		waiters: make(map[chan struct{}]string),
	}
}

func (n *Notifier) Wait(ctx context.Context, agent string) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.waiters[ch] = agent
	n.mu.Unlock()

	defer func() {
		n.mu.Lock()
		delete(n.waiters, ch)
		n.mu.Unlock()
	}()

	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// notifyAll wakes waiters that could be interested in a message addressed to `to`.
// Broadcasts (to==nil) and group messages (to starts with '@') wake everyone since
// group membership can't be resolved without a DB query.
func (n *Notifier) notifyAll(to *string) {
	n.mu.Lock()
	for ch, agent := range n.waiters {
		if to == nil || len(*to) > 0 && (*to)[0] == '@' || *to == agent {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	n.mu.Unlock()
}

func (n *Notifier) Listen(ctx context.Context, pool *pgxpool.Pool) {
	go func() {
		for {
			if err := n.listenLoop(ctx, pool); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("notifier: reconnecting after error: %v", err)
			}
		}
	}()
}

func (n *Notifier) listenLoop(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN new_message"); err != nil {
		return err
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}

		var payload struct {
			ID      int     `json:"id"`
			Channel string  `json:"channel"`
			To      *string `json:"to"`
		}
		_ = json.Unmarshal([]byte(notification.Payload), &payload)

		n.notifyAll(payload.To)
	}
}

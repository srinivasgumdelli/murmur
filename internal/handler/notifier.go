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
	waiters map[chan struct{}]struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{
		waiters: make(map[chan struct{}]struct{}),
	}
}

func (n *Notifier) Wait(ctx context.Context) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.waiters[ch] = struct{}{}
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

func (n *Notifier) notifyAll() {
	n.mu.Lock()
	for ch := range n.waiters {
		select {
		case ch <- struct{}{}:
		default:
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
			ID      int    `json:"id"`
			Channel string `json:"channel"`
		}
		_ = json.Unmarshal([]byte(notification.Payload), &payload)

		n.notifyAll()
	}
}

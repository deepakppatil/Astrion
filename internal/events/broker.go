// Package events fans out job-event notifications to live subscribers (SSE
// streams). Durability comes from the job_events outbox table: a
// notification is only a wake-up hint, and subscribers always re-read the
// table from their last seen event ID. Missed notifications therefore cause
// latency, never data loss (OBS-002, ADR-0003).
package events

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Broker listens on a PostgreSQL NOTIFY channel and wakes subscribers whose
// job ID matches the notification payload.
type Broker struct {
	pool    *pgxpool.Pool
	channel string
	log     *slog.Logger

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
	all  map[chan struct{}]struct{}
}

func NewBroker(pool *pgxpool.Pool, channel string, log *slog.Logger) *Broker {
	if log == nil {
		log = slog.Default()
	}
	return &Broker{pool: pool, channel: channel, log: log,
		subs: map[string]map[chan struct{}]struct{}{}, all: map[chan struct{}]struct{}{}}
}

// Subscribe returns a channel that receives a value (coalesced) whenever an
// event is appended for jobID. An empty jobID subscribes to all jobs.
func (b *Broker) Subscribe(jobID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if jobID == "" {
		b.all[ch] = struct{}{}
	} else {
		if b.subs[jobID] == nil {
			b.subs[jobID] = map[chan struct{}]struct{}{}
		}
		b.subs[jobID][ch] = struct{}{}
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if jobID == "" {
			delete(b.all, ch)
		} else if m := b.subs[jobID]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				delete(b.subs, jobID)
			}
		}
	}
}

// Publish wakes subscribers for jobID (used by the listener and in tests).
func (b *Broker) Publish(jobID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wake := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default: // already pending; coalesce
		}
	}
	for ch := range b.subs[jobID] {
		wake(ch)
	}
	for ch := range b.all {
		wake(ch)
	}
}

// wakeAll is used after reconnecting, because notifications may have been
// missed while disconnected.
func (b *Broker) wakeAll() {
	b.mu.Lock()
	ids := make([]string, 0, len(b.subs))
	for id := range b.subs {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.Publish(id)
	}
}

// Run holds a dedicated connection with LISTEN until ctx is cancelled,
// reconnecting with backoff.
func (b *Broker) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := b.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("event listener disconnected; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (b *Broker) listen(ctx context.Context) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+quoteIdent(b.channel)); err != nil {
		return err
	}
	defer func() {
		// Do not return a LISTENing connection to the pool.
		c := context.Background()
		_, _ = conn.Exec(c, "UNLISTEN *")
	}()
	b.wakeAll()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		b.Publish(n.Payload)
	}
}

func quoteIdent(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}

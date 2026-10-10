package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/deepakppatil/astrion/internal/events"
	"github.com/deepakppatil/astrion/internal/store"
	"github.com/deepakppatil/astrion/internal/testutil"
)

func TestPublishCoalescesAndFilters(t *testing.T) {
	b := events.NewBroker(nil, "x", nil)
	a, cancelA := b.Subscribe("job_a")
	defer cancelA()
	all, cancelAll := b.Subscribe("")
	defer cancelAll()
	b.Publish("job_b")
	b.Publish("job_b")
	select {
	case <-a:
		t.Fatal("job_a subscriber woken by job_b")
	default:
	}
	select {
	case <-all:
	default:
		t.Fatal("wildcard subscriber not woken")
	}
	b.Publish("job_a")
	b.Publish("job_a") // coalesced, must not block
	<-a
}

func TestListenNotify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := store.Open(ctx, testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := events.NewBroker(s.Pool, store.NotifyChannel, nil)
	ch, unsub := b.Subscribe("job_1")
	defer unsub()
	done := make(chan struct{})
	runCtx, stop := context.WithCancel(ctx)
	go func() { b.Run(runCtx); close(done) }()
	defer func() { stop(); <-done }() // release the LISTEN connection before Close
	<-ch                              // initial wake after LISTEN is established
	if _, err := s.Pool.Exec(ctx, `SELECT pg_notify($1, 'job_1')`, store.NotifyChannel); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("notification not received")
	}
}

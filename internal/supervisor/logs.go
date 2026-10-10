package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/redact"
)

const (
	maxLineBytes  = 8 << 10
	maxBatchLines = 200
	maxBatchBytes = 60 << 10
	maxBuffered   = 20000
	flushInterval = 500 * time.Millisecond
)

// logShipper batches, redacts, and ships log lines. Every line is redacted
// before it leaves the execution environment (AUTH-002).
type logShipper struct {
	api API
	red *redact.Redactor
	log *slog.Logger

	mu      sync.Mutex
	buf     []execapi.LogLine
	dropped int
	seq     int64
	wake    chan struct{}
	sendMu  sync.Mutex
}

func newLogShipper(api API, red *redact.Redactor, log *slog.Logger) *logShipper {
	return &logShipper{api: api, red: red, log: log, wake: make(chan struct{}, 1)}
}

func (l *logShipper) add(stream, level, msg string) {
	if l == nil {
		return
	}
	if len(msg) > maxLineBytes {
		msg = msg[:maxLineBytes] + " …[truncated]"
	}
	line := execapi.LogLine{Time: time.Now().UTC(), Stream: stream, Level: level, Message: l.red.String(msg)}
	l.mu.Lock()
	if len(l.buf) >= maxBuffered {
		l.dropped++
	} else {
		l.buf = append(l.buf, line)
	}
	full := len(l.buf) >= maxBatchLines
	l.mu.Unlock()
	if full {
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}
}

func (l *logShipper) run(ctx context.Context) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.wake:
		}
		l.flush(ctx)
	}
}

// flush sends all buffered lines in bounded batches.
func (l *logShipper) flush(ctx context.Context) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	for {
		l.mu.Lock()
		if l.dropped > 0 {
			l.buf = append(l.buf, execapi.LogLine{Time: time.Now().UTC(), Stream: "supervisor", Level: "warn",
				Message: fmt.Sprintf("%d log lines dropped (buffer full)", l.dropped)})
			l.dropped = 0
		}
		if len(l.buf) == 0 {
			l.mu.Unlock()
			return
		}
		n, size := 0, 0
		for n < len(l.buf) && n < maxBatchLines && size < maxBatchBytes {
			size += len(l.buf[n].Message) + 64
			n++
		}
		batch := append([]execapi.LogLine(nil), l.buf[:n]...)
		l.buf = l.buf[n:]
		l.seq++
		seq := l.seq
		l.mu.Unlock()

		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := l.api.Logs(sctx, execapi.LogBatch{Seq: seq, Lines: batch})
		cancel()
		if err != nil {
			l.log.Warn("log shipping failed; batch dropped", "err", err, "lines", len(batch))
			return
		}
	}
}

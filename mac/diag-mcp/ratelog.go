package main

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// rateLog writes at most one line per window for each kind of event, with
// how many lines it held back. The server is on the internet: a stranger
// sending bad tokens or broken TLS handshakes must not fill the disk.
type rateLog struct {
	log    *slog.Logger
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time
	dropped map[string]int
}

func newRateLog(log *slog.Logger, window time.Duration) *rateLog {
	return &rateLog{log: log, window: window, now: time.Now, last: map[string]time.Time{}, dropped: map[string]int{}}
}

func (r *rateLog) warn(msg string, args ...any) {
	r.mu.Lock()
	now := r.now()
	if at, ok := r.last[msg]; ok && now.Sub(at) < r.window {
		r.dropped[msg]++
		r.mu.Unlock()
		return
	}
	r.last[msg] = now
	held := r.dropped[msg]
	r.dropped[msg] = 0
	r.mu.Unlock()
	if held > 0 {
		args = append(args, "held_back", held)
	}
	r.log.Warn(msg, args...)
}

// writer is an io.Writer for http.Server's ErrorLog: each line goes through
// warn under one kind, cut to 200 bytes, since a client chose some of it.
func (r *rateLog) writer(kind string) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		line := strings.TrimSpace(string(p))
		if len(line) > 200 {
			line = line[:200]
		}
		r.warn(kind, "err", line)
		return len(p), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

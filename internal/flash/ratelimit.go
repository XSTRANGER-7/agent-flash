package flash

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limiter is a token bucket (stdlib only).
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func NewLimiter(rps float64, burst int) *Limiter {
	return &Limiter{rate: rps, burst: float64(burst), tokens: float64(burst), last: time.Now()}
}

// SetRate changes the rate (e.g. from a RateLimit-Policy header).
func (l *Limiter) SetRate(rps float64, burst int) {
	if rps <= 0 || burst <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate, l.burst = rps, float64(burst)
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
}

func (l *Limiter) Rate() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Wait blocks until a token is available or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// parsePolicy reads an IETF RateLimit-Policy value such as "5;w=1" and
// returns requests per second and the bucket size.
func parsePolicy(v string) (rps float64, burst int, ok bool) {
	v = strings.TrimSpace(strings.Split(v, ",")[0]) // first policy wins
	if v == "" {
		return 0, 0, false
	}
	parts := strings.Split(v, ";")
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	window := 1.0
	for _, p := range parts[1:] {
		if k, val, found := strings.Cut(strings.TrimSpace(p), "="); found && strings.TrimSpace(k) == "w" {
			if w, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && w > 0 {
				window = w
			}
		}
	}
	return float64(n) / window, n, true
}

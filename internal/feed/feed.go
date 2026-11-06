// Package feed is the live social feed (reasoning posts, blocks, copies,
// approvals) with fan-out to Server-Sent Events subscribers.
package feed

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type Post struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"` // trade_placed | trade_blocked | trade_closed | copy_placed | copy_blocked | approval
	AgentID   string         `json:"agentId,omitempty"`
	TradeID   string         `json:"tradeId,omitempty"`
	Text      string         `json:"text"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

type Feed struct {
	mu   sync.RWMutex
	ring []Post
	max  int
	subs map[chan Post]struct{}
}

func New(max int) *Feed { return &Feed{max: max, subs: map[chan Post]struct{}{}} }

func id() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "post_" + hex.EncodeToString(b)
}

func (f *Feed) Publish(p Post) Post {
	if p.ID == "" {
		p.ID = id()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	f.mu.Lock()
	f.ring = append(f.ring, p)
	if len(f.ring) > f.max {
		f.ring = f.ring[len(f.ring)-f.max:]
	}
	for ch := range f.subs {
		select {
		case ch <- p:
		default: // slow subscriber: drop rather than block trading
		}
	}
	f.mu.Unlock()
	return p
}

// Latest returns newest first.
func (f *Feed) Latest(n int) []Post {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Post, 0, n)
	for i := len(f.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, f.ring[i])
	}
	return out
}

func (f *Feed) Subscribe() (chan Post, func()) {
	ch := make(chan Post, 64)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

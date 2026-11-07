// Package feed is the social feed (reasoning posts, blocks, copies,
// approvals) with fan-out to Server-Sent Events subscribers. Open keeps it
// on disk too: every post is appended to a JSON-lines file and the history
// comes back after a restart.
package feed

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/atomicfile"
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
	ring []Post // oldest first; the newest max posts
	max  int
	subs map[chan Post]struct{}
	file *os.File // append-only history; nil keeps posts in memory only
	werr error    // last write error, for health checks
}

// New is an in-memory feed that keeps the newest max posts.
func New(max int) *Feed { return &Feed{max: max, subs: map[chan Post]struct{}{}} }

// compactAbove is the file size past which Open rewrites the history with
// only the posts it keeps, so the file cannot grow without bound.
const compactAbove = 32 << 20

// Open loads the saved history from path (one JSON post per line), keeps the
// newest max posts in memory, and appends every new post to the file.
func Open(path string, max int) (*Feed, error) {
	f := New(max)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var p Post
			if json.Unmarshal(sc.Bytes(), &p) == nil && p.ID != "" { // a crash can leave half a line: skipped
				f.ring = append(f.ring, p)
			}
		}
		if len(f.ring) > max {
			f.ring = f.ring[len(f.ring)-max:]
		}
		if len(b) > compactAbove {
			var out bytes.Buffer
			for _, p := range f.ring {
				line, _ := json.Marshal(p)
				out.Write(append(line, '\n'))
			}
			if err := atomicfile.Write(path, out.Bytes(), 0o600); err != nil {
				return nil, err
			}
			b = out.Bytes()
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		_, _ = fh.Write([]byte{'\n'}) // end a half-written line before appending
	}
	f.file = fh
	return f, nil
}

// Close flushes and closes the history file.
func (f *Feed) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	err := f.file.Sync()
	if cerr := f.file.Close(); err == nil {
		err = cerr
	}
	f.file = nil
	return err
}

// WriteErr is the last error writing the history file, if any.
func (f *Feed) WriteErr() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.werr
}

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
	if f.file != nil {
		if line, err := json.Marshal(p); err == nil {
			if _, err := f.file.Write(append(line, '\n')); err != nil {
				f.werr = err
			}
		}
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
	out, _, _ := f.Page("", n, nil)
	return out
}

// Page returns up to n posts that match keep, newest first, older than the
// post with id before ("" = from the newest). more says whether older
// matching posts exist; total counts every matching post kept.
func (f *Feed) Page(before string, n int, keep func(Post) bool) (out []Post, more bool, total int) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	start := len(f.ring) - 1
	if before != "" {
		start = -1
		for i := len(f.ring) - 1; i >= 0; i-- {
			if f.ring[i].ID == before {
				start = i - 1
				break
			}
		}
	}
	out = make([]Post, 0, min(n, 256))
	for i := len(f.ring) - 1; i >= 0; i-- {
		p := f.ring[i]
		if keep != nil && !keep(p) {
			continue
		}
		total++
		if i > start {
			continue
		}
		if len(out) < n {
			out = append(out, p)
		} else {
			more = true
		}
	}
	return out, more, total
}

// Oldest is when the oldest kept post was made (zero when empty).
func (f *Feed) Oldest() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.ring) == 0 {
		return time.Time{}
	}
	return f.ring[0].CreatedAt
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

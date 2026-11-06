// Package outbox reliably mirrors public records to Arkiv.
//
// Pattern: every public event is appended to a local JSONL log first
// (durable), then a background worker pushes it to the arkiv-sync sidecar,
// which writes an Arkiv entity with the official TypeScript SDK. Delivered
// IDs are recorded so restarts never double-publish and a sidecar outage
// never loses data or blocks trading.
package outbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/metrics"
)

// Event becomes one Arkiv entity. Attributes are the queryable fields
// (Arkiv requires lowercase attribute keys). TTL is the entity lifetime.
type Event struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"` // agent | trade | signal | prediction | season
	Attributes map[string]any `json:"attributes"`
	Payload    any            `json:"payload"`
	TTLHours   int            `json:"ttlHours"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type Outbox struct {
	log     *slog.Logger
	dir     string
	url     string
	token   string
	http    *http.Client
	mu      sync.Mutex
	pending []Event
	sent    map[string]bool
	logFile *os.File
	sentF   *os.File
	wake    chan struct{}
}

func Open(dir, sidecarURL, token string, log *slog.Logger) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	o := &Outbox{log: log, dir: dir, url: sidecarURL, token: token, http: &http.Client{Timeout: 20 * time.Second}, sent: map[string]bool{}, wake: make(chan struct{}, 1)}
	// Load delivered IDs.
	if f, err := os.Open(filepath.Join(dir, "outbox.sent")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			o.sent[sc.Text()] = true
		}
		f.Close()
	}
	// Replay undelivered events.
	if f, err := os.Open(filepath.Join(dir, "outbox.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil && !o.sent[e.ID] {
				o.pending = append(o.pending, e)
			}
		}
		f.Close()
	}
	var err error
	if o.logFile, err = os.OpenFile(filepath.Join(dir, "outbox.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	if o.sentF, err = os.OpenFile(filepath.Join(dir, "outbox.sent"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil {
		return nil, err
	}
	return o, nil
}

// Publish durably records an event and schedules delivery. Never blocks on network.
func (o *Outbox) Publish(e Event) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(e)
	o.mu.Lock()
	if _, err := o.logFile.Write(append(b, '\n')); err != nil {
		o.log.Error("outbox write failed", "err", err)
	}
	o.pending = append(o.pending, e)
	o.mu.Unlock()
	metrics.Inc("outbox_events_total")
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Run delivers pending events until ctx ends. No-op when no sidecar is set.
func (o *Outbox) Run(ctx context.Context) {
	if o.url == "" {
		o.log.Info("outbox: ARKIV_SYNC_URL not set; events are kept locally only")
		return
	}
	backoff := time.Second
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		o.mu.Lock()
		batch := append([]Event(nil), o.pending...)
		o.mu.Unlock()
		failed := false
		for _, e := range batch {
			if err := o.deliver(ctx, e); err != nil {
				o.log.Warn("outbox delivery failed; will retry", "id", e.ID, "err", err)
				metrics.Inc("outbox_failures_total")
				failed = true
				break
			}
			o.markSent(e.ID)
		}
		if failed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-tick.C:
		}
	}
}

func (o *Outbox) deliver(ctx context.Context, e Event) error {
	b, _ := json.Marshal(e)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/events", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	res, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("sidecar status %d", res.StatusCode)
	}
	return nil
}

func (o *Outbox) markSent(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent[id] = true
	_, _ = o.sentF.WriteString(id + "\n")
	out := o.pending[:0]
	for _, e := range o.pending {
		if e.ID != id {
			out = append(out, e)
		}
	}
	o.pending = out
	metrics.Set("outbox_pending", float64(len(o.pending)))
}

func (o *Outbox) Pending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

func (o *Outbox) Close() {
	o.logFile.Close()
	o.sentF.Close()
}

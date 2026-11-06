// Package approval implements human-in-the-loop approval for large trades.
//
// Flow: the engine calls Await(); a pending request is created and shown in
// the app; the human approves or rejects (World ID Agent Pass in production,
// admin API in development); on timeout the request EXPIRES and the trade does
// not happen. Every path is recorded, which is what the World track asks for.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Rejected Status = "rejected"
	Expired  Status = "expired"
)

type Request struct {
	ID        string    `json:"id"`
	WalletID  string    `json:"walletId"`
	AgentID   string    `json:"agentId"`
	AmountUSD float64   `json:"amountUsd"`
	Summary   string    `json:"summary"`
	Status    Status    `json:"status"`
	Method    string    `json:"method,omitempty"` // "world-agent-pass" | "admin"
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	DecidedAt time.Time `json:"decidedAt,omitempty"`
	done      chan struct{}
}

// Verifier validates a World ID Agent Pass result on the BACKEND. Never
// trust a client-side "approved" flag. Implement this with World's server
// verification for the hackathon World track.
type Verifier interface {
	Verify(ctx context.Context, requestID string, proof []byte) (bool, error)
}

type Manager struct {
	mu       sync.Mutex
	reqs     map[string]*Request
	timeout  time.Duration
	verifier Verifier
	onChange func(*Request)
}

func NewManager(timeout time.Duration, v Verifier, onChange func(*Request)) *Manager {
	return &Manager{reqs: map[string]*Request{}, timeout: timeout, verifier: v, onChange: onChange}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "apr_" + hex.EncodeToString(b)
}

// Await blocks until the request is decided, expires, or ctx ends.
func (m *Manager) Await(ctx context.Context, walletID, agentID string, usd float64, summary string) (*Request, error) {
	r := &Request{ID: newID(), WalletID: walletID, AgentID: agentID, AmountUSD: usd, Summary: summary,
		Status: Pending, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(m.timeout), done: make(chan struct{})}
	m.mu.Lock()
	m.reqs[r.ID] = r
	m.mu.Unlock()
	m.notify(r)
	t := time.NewTimer(m.timeout)
	defer t.Stop()
	select {
	case <-r.done:
	case <-t.C:
		m.decide(r.ID, Expired, "timeout")
	case <-ctx.Done():
		m.decide(r.ID, Expired, "cancelled")
	}
	return m.Get(r.ID), nil
}

var ErrNotPending = errors.New("approval is not pending")

func (m *Manager) decide(id string, s Status, method string) (*Request, error) {
	m.mu.Lock()
	r, ok := m.reqs[id]
	if !ok {
		m.mu.Unlock()
		return nil, errors.New("approval not found")
	}
	if r.Status != Pending {
		m.mu.Unlock()
		return r, ErrNotPending
	}
	r.Status, r.Method, r.DecidedAt = s, method, time.Now().UTC()
	close(r.done)
	cp := *r
	m.mu.Unlock()
	m.notify(&cp)
	return &cp, nil
}

// AdminDecide is the development path (bearer-token protected API).
func (m *Manager) AdminDecide(id string, approve bool) (*Request, error) {
	s := Rejected
	if approve {
		s = Approved
	}
	return m.decide(id, s, "admin")
}

// WorldDecide verifies an Agent Pass proof on the server, then decides.
func (m *Manager) WorldDecide(ctx context.Context, id string, approve bool, proof []byte) (*Request, error) {
	if !approve {
		return m.decide(id, Rejected, "world-agent-pass")
	}
	if m.verifier == nil {
		return nil, errors.New("world verifier not configured")
	}
	ok, err := m.verifier.Verify(ctx, id, proof)
	if err != nil {
		return nil, err
	}
	if !ok {
		return m.decide(id, Rejected, "world-agent-pass-invalid")
	}
	return m.decide(id, Approved, "world-agent-pass")
}

func (m *Manager) Get(id string) *Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.reqs[id]; ok {
		cp := *r
		return &cp
	}
	return nil
}

func (m *Manager) List(onlyPending bool) []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Request{}
	for _, r := range m.reqs {
		if !onlyPending || r.Status == Pending {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Manager) notify(r *Request) {
	if m.onChange != nil {
		m.onChange(r)
	}
}

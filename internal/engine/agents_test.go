package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/store"
)

// A person adds an agent while the arena runs: the published agent map is
// never changed in place, the new agent starts its own loop, and it trades
// its own signal from its maker's account with the maker's stop loss.
func TestPeopleCanAddAgentsWhileRunning(t *testing.T) {
	e, ff := setup(t)
	e.Cfg.LiveAgents, e.Cfg.AgentEvery = true, 40*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunAgents(ctx)
	time.Sleep(30 * time.Millisecond)

	before := e.AgentMap()
	ua := store.UserAgent{ID: "follower1", Name: "Kaka Junior", Owner: "0x000000000000000000000000000000000000dEaD", Kind: "contrarian",
		Streak: 2, TPPct: 1.5, SLPct: 1, SizePct: 25, CreatedAt: time.Now().UTC()}
	prof, err := UserAgentProfile(ua)
	if err != nil {
		t.Fatal(err)
	}
	e.Store.PutUserAgent(ua)
	e.AddAgent(prof)
	if _, leaked := before["follower1"]; leaked {
		t.Fatal("a published agent map was changed in place")
	}
	if p, ok := e.AgentMap()["follower1"]; !ok || !p.Community || p.Owner != ua.Owner {
		t.Fatalf("agent not registered as a community agent: %+v", p)
	}

	for _, px := range []float64{3000, 2990, 2980} { // two falling checks in a row
		ff.mu.Lock()
		ff.searchPx = px
		ff.mu.Unlock()
		e.Prices.Refresh(ctx)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, tr := range e.Store.Trades("follower1", 0) {
			if tr.Kind != "agent" || tr.WalletID != "follower1" {
				continue
			}
			if want := math.Round(2980*0.99*100) / 100; math.Abs(tr.StopLoss-want) > 0.02 {
				t.Fatalf("stop loss %.2f, want the maker's 1%% (%.2f)", tr.StopLoss, want)
			}
			if tr.SizeUSD != 5 { // 25% of the account's $20
				t.Fatalf("size %.2f, want 25%% of $20", tr.SizeUSD)
			}
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("the new agent never traded its signal")
}

// A paused agent does not trade.
func TestPausedUserAgentDoesNotTrade(t *testing.T) {
	e, ff := setup(t)
	e.Cfg.LiveAgents, e.Cfg.AgentEvery = true, 30*time.Millisecond
	ua := store.UserAgent{ID: "follower1", Name: "Sleepy", Owner: "0x000000000000000000000000000000000000dEaD", Kind: "contrarian",
		Streak: 2, TPPct: 1.5, SLPct: 1, SizePct: 25, Paused: true, CreatedAt: time.Now().UTC()}
	e.Store.PutUserAgent(ua)
	e.LoadUserAgents()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunAgents(ctx)
	for _, px := range []float64{3000, 2990, 2980} {
		ff.mu.Lock()
		ff.searchPx = px
		ff.mu.Unlock()
		e.Prices.Refresh(ctx)
	}
	time.Sleep(400 * time.Millisecond)
	for _, tr := range e.Store.Trades("follower1", 0) {
		if tr.Kind == "agent" {
			t.Fatal("a paused agent traded")
		}
	}
}

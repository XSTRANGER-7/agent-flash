package engine

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/referee"
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

// Copy followers of a community agent copy its trades. Its wallet is the
// maker's account (role follower), which the copy fan-out once ignored.
func TestCommunityAgentTradesAreCopied(t *testing.T) {
	e, ff := setup(t)
	ua := store.UserAgent{ID: "follower1", Name: "Kaku", Owner: "0x000000000000000000000000000000000000dEaD", Kind: "contrarian",
		Streak: 2, TPPct: 1.5, SLPct: 1, SizePct: 25, CreatedAt: time.Now().UTC()}
	e.Store.PutUserAgent(ua)
	e.LoadUserAgents()
	cw, err := e.Wallets.Create("copier")
	if err != nil {
		t.Fatal(err)
	}
	ff.mu.Lock()
	ff.usdc[strings.ToLower(cw.Address())] = 12
	ff.mu.Unlock()
	e.Store.PutFollow(&store.Follow{FollowerID: "copier", AgentID: "follower1", Mode: "copy", AllocationPct: 25, MaxPerTradeUSD: 3, DailyCapUSD: 10, Active: true})

	in := referee.Intent{AgentID: "follower1", WalletID: "follower1", Kind: "agent", OrderType: "market+bracket", Token: "WETH",
		SizePct: 25, Price: 3000, TakeProfit: 3045, StopLoss: 2970, Reason: "test"}
	if res := e.Propose(context.Background(), in); res.Trade == nil {
		t.Fatalf("agent trade: %+v", res.Decision)
	}
	e.Wait()
	var cp *store.Trade
	for _, tr := range e.Store.Trades("follower1", 0) {
		if tr.WalletID == "copier" {
			tc := tr
			cp = &tc
		}
	}
	if cp == nil || cp.CopyOf == "" || cp.SizeUSD != 3 || cp.StopLoss != 2970 {
		t.Fatalf("community agent trade not copied: %+v", cp)
	}
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

// An agent maker's Advanced settings tighten its trades (size, trades a day)
// and values past the hard caps are refused.
func TestAgentLimitsApplyAndAreCapped(t *testing.T) {
	e, _ := setup(t)
	if _, err := CheckAgentLimits(e.Cfg, store.AgentLimits{MaxTradeUSD: e.Cfg.MaxTradeUSD + 1}); err == nil {
		t.Fatal("a largest trade above the arena's was accepted")
	}
	if _, err := CheckAgentLimits(e.Cfg, store.AgentLimits{MaxCostPct: 9}); err == nil {
		t.Fatal("a 9% cost limit was accepted")
	}
	l, err := CheckAgentLimits(e.Cfg, store.AgentLimits{MaxTradeUSD: 2, TradesPerDay: 1, BreakEven: "off"})
	if err != nil {
		t.Fatal(err)
	}
	in := referee.Intent{AgentID: "follower1", WalletID: "follower1", Kind: "agent", OrderType: "market+bracket", Token: "WETH",
		SizePct: 25, Price: 3000, TakeProfit: 3045, StopLoss: 2970, Reason: "test"}
	applyLimits(&in, l)
	res := e.Propose(context.Background(), in)
	if res.Trade == nil || res.Trade.SizeUSD != 2 || res.Trade.BreakEven != "off" {
		t.Fatalf("first trade: %+v %+v", res.Decision, res.Trade)
	}
	e.Referee.Closed(in.WalletID) // close the position so only the daily limit can stop the next one
	if res := e.Propose(context.Background(), in); res.Decision.Code != "DAILY_LIMIT" {
		t.Fatalf("want DAILY_LIMIT after its own 1 trade a day, got %+v", res.Decision)
	}
}

// Advanced settings can only be stricter than the arena: followers trade
// under the agent's cost and slippage limits.
func TestAgentLimitsNeverLoosenTheArena(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.MaxEntryCostPct, e.Cfg.MaxSlippage, e.Cfg.MaxTradesPerDay = 1.2, "0.01", 5
	for _, l := range []store.AgentLimits{{MaxCostPct: 1.5}, {SlippagePct: 2}, {TradesPerDay: 6}} {
		if _, err := CheckAgentLimits(e.Cfg, l); err == nil {
			t.Fatalf("accepted a looser limit than the arena: %+v", l)
		}
	}
	if _, err := CheckAgentLimits(e.Cfg, store.AgentLimits{MaxCostPct: 1, SlippagePct: 0.5, TradesPerDay: 3}); err != nil {
		t.Fatalf("stricter limits refused: %v", err)
	}
}

// Retiring an agent removes it from the arena, unlinks its followers, and
// its loop stops; the same account can make a new agent later.
func TestRetireAgent(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.AgentEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunAgents(ctx)
	time.Sleep(10 * time.Millisecond)
	ua := store.UserAgent{ID: "follower1", Name: "Old", Owner: "0x000000000000000000000000000000000000dEaD", Kind: "contrarian",
		Streak: 2, TPPct: 1.5, SLPct: 1, SizePct: 25, CreatedAt: time.Now().UTC()}
	prof, _ := UserAgentProfile(ua)
	e.Store.PutUserAgent(ua)
	e.AddAgent(prof)
	e.Store.PutFollow(&store.Follow{FollowerID: "trendhunter", AgentID: "follower1", Mode: "copy", Active: true})
	e.agentsMu.Lock()
	gen := e.agentGen["follower1"]
	e.agentsMu.Unlock()
	if n := e.RetireAgent("follower1"); n != 1 {
		t.Fatalf("unlinked %d followers, want 1", n)
	}
	if _, ok := e.AgentMap()["follower1"]; ok {
		t.Fatal("retired agent still listed")
	}
	if _, ok := e.Store.UserAgent("follower1"); ok {
		t.Fatal("retired agent still stored")
	}
	if len(e.Store.Followers("follower1")) != 0 {
		t.Fatal("followers not unlinked")
	}
	if e.current("follower1", gen) {
		t.Fatal("the retired agent's loop would keep running")
	}
	e.AddAgent(prof) // make it again
	if _, ok := e.AgentMap()["follower1"]; !ok {
		t.Fatal("could not make the agent again")
	}
}

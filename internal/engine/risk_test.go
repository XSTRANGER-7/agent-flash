package engine

import (
	"context"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
)

func pnl(v float64) *float64 { return &v }

// A 5% swing in the price window blocks automated entries; a person's manual
// order is not an automated entry and is not stopped by this guard.
func TestVolatilityGuardBlocksAutomatedEntries(t *testing.T) {
	e, ff := setup(t)
	e.Cfg.MaxMovePct, e.Cfg.MoveChecks = 3, 60
	for _, px := range []float64{3000, 3150} {
		ff.mu.Lock()
		ff.searchPx = px
		ff.mu.Unlock()
		e.Prices.Refresh(context.Background())
	}
	if res := e.Propose(context.Background(), intent()); res.Decision.Code != "VOLATILE" {
		t.Fatalf("agent entry: want VOLATILE, got %+v", res.Decision)
	}
	in := intent()
	in.WalletID = "follower1" // not the agent's own wallet, not a copy: a manual order
	if res := e.Propose(context.Background(), in); res.Decision.Code == "VOLATILE" {
		t.Fatal("a manual order was stopped by the volatility guard")
	}
	if m := PriceMove([]float64{100, 103, 101}, 60); m < 2.99 || m > 3.01 {
		t.Fatalf("PriceMove = %v, want 3", m)
	}
}

// Three losses in a row pause the wallet's automated entries for the pause
// window; a win in between resets the streak.
func TestLossStreakPausesAWallet(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.MaxLossStreak, e.Cfg.LossStreakPause = 3, time.Hour
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		e.Store.PutTrade(&store.Trade{ID: "loss" + string(rune('a'+i)), AgentID: "trendhunter", WalletID: "trendhunter", Kind: "agent",
			Status: "closed", PnLUSD: pnl(-0.1), CreatedAt: now.Add(-time.Duration(10-i) * time.Minute), UpdatedAt: now.Add(-time.Duration(9-i) * time.Minute)})
	}
	if res := e.Propose(context.Background(), intent()); res.Decision.Code != "LOSS_STREAK" {
		t.Fatalf("want LOSS_STREAK after 3 losses, got %+v", res.Decision)
	}
	e.Store.PutTrade(&store.Trade{ID: "win", AgentID: "trendhunter", WalletID: "trendhunter", Kind: "agent",
		Status: "closed", PnLUSD: pnl(0.2), CreatedAt: now.Add(-time.Minute), UpdatedAt: now})
	if n, _ := LossStreak(e.Store.Trades("", 0), "trendhunter"); n != 0 {
		t.Fatalf("a win must reset the streak, got %d", n)
	}
	if res := e.Propose(context.Background(), intent()); res.Decision.Code == "LOSS_STREAK" {
		t.Fatal("still paused after a win")
	}
}

// Real-money losses today past the limit stop new entries; paper losses don't count.
func TestDailyLossLimit(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.DailyLossLimitUSD = 1
	e.Cfg.Paper, e.Cfg.DryRun = false, false // live
	now := time.Now()
	e.Store.PutTrade(&store.Trade{ID: "p1", WalletID: "trendhunter", Kind: "agent", Status: "closed", Paper: true, PnLUSD: pnl(-5), CreatedAt: now, UpdatedAt: now})
	if d := e.dailyLossGate(); !d.OK {
		t.Fatalf("paper losses counted: %+v", d)
	}
	e.Store.PutTrade(&store.Trade{ID: "l1", WalletID: "trendhunter", Kind: "agent", Status: "closed", PnLUSD: pnl(-0.6), CreatedAt: now, UpdatedAt: now})
	e.Store.PutTrade(&store.Trade{ID: "l2", WalletID: "follower1", Kind: "copy", Status: "closed", PnLUSD: pnl(-0.5), CreatedAt: now, UpdatedAt: now})
	if d := e.dailyLossGate(); d.OK || d.Code != "DAILY_LOSS" {
		t.Fatalf("want DAILY_LOSS at -$1.10 with a $1 limit, got %+v", d)
	}
	in := referee.Intent{WalletID: "trendhunter", AgentID: "trendhunter"}
	w, _ := e.Wallets.Get("trendhunter")
	if d := e.riskGate(in, w, ""); d.Code != "DAILY_LOSS" {
		t.Fatalf("risk gate: %+v", d)
	}
}

// Automated positions past their time limit are listed for a market exit;
// manual ones and fresh ones are not.
func TestOverdueAutomatedPositions(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.MaxHoldHours = 1
	now := time.Now()
	e.Store.PutTrade(&store.Trade{ID: "own", Kind: "agent", Status: "filled", MaxHoldMin: 30, CreatedAt: now.Add(-45 * time.Minute)})
	e.Store.PutTrade(&store.Trade{ID: "plan", Kind: "plan", Status: "filled", CreatedAt: now.Add(-2 * time.Hour)})
	e.Store.PutTrade(&store.Trade{ID: "fresh", Kind: "agent", Status: "filled", CreatedAt: now.Add(-20 * time.Minute)})
	e.Store.PutTrade(&store.Trade{ID: "manual", Kind: "manual", Status: "filled", CreatedAt: now.Add(-5 * time.Hour)})
	e.Store.PutTrade(&store.Trade{ID: "closed", Kind: "agent", Status: "closed", CreatedAt: now.Add(-5 * time.Hour)})
	got := map[string]bool{}
	for _, tr := range e.Overdue(now) {
		got[tr.ID] = true
	}
	if !got["own"] || !got["plan"] || got["fresh"] || got["manual"] || got["closed"] || len(got) != 2 {
		t.Fatalf("overdue = %v", got)
	}
}

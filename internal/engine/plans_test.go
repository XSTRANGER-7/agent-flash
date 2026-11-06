package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
)

// planSetup: paper mode with cbBTC tradeable and priced by a QuoteFeed.
func planSetup(t *testing.T) (*Engine, *arena.QuoteFeed) {
	t.Helper()
	e, _, v, px := paperSetup(t)
	e.Venue = v
	_ = px
	e.Cfg.Tokens = []config.Token{{Symbol: "WETH", Address: wethAddr, Decimals: 18}, {Symbol: "cbBTC", Address: cbbtcAddr, Decimals: 8}}
	e.Referee = referee.New(referee.Policy{AllowedTokens: map[string]bool{"WETH": true, "cbBTC": true}, MaxTradeUSD: 100, MaxPositionPct: 25,
		MaxTradesPerDay: 5, MaxOpenPerAgent: 1, MinTPPct: 0.5, MaxSLPct: 5, MinLiquidityUSD: 1_000_000, MaxDrawdownPct: 20})
	e.Cfg.MaxTradeUSD = 100
	qf := arena.NewQuoteFeed(e.Flash, "cbBTC", cbbtcAddr, usdcAddr, "base", "30", e.Log)
	qf.Refresh(context.Background())
	if qf.Last() <= 0 {
		t.Fatalf("quote feed has no price: %s", qf.Err())
	}
	e.TokenFeeds = map[string]*arena.QuoteFeed{strings.ToLower(cbbtcAddr): qf}
	return e, qf
}

func newPlan(id, wallet, strategy string, mut func(*store.Plan)) store.Plan {
	p := store.Plan{ID: id, Name: "test plan", Owner: "Vikram", WalletID: wallet, Token: "cbBTC", Strategy: strategy, IntervalMin: 1,
		TradeUSD: 8, BudgetUSD: 16, TakeProfitPct: 2, StopLossPct: 1, MaxSlippagePct: 1, MaxTradesPerDay: 5, LossLimitPct: 100,
		StartsAt: time.Now().Add(-time.Minute), EndsAt: time.Now().Add(time.Hour), Status: "active", CreatedAt: time.Now()}
	if mut != nil {
		mut(&p)
	}
	return p
}

func TestPlanTradesItsOwnCoinSizeAndTargets(t *testing.T) {
	e, _ := planSetup(t)
	ctx := context.Background()
	e.Store.PutPlan(newPlan("pln_1", "follower1", "interval", nil))
	e.StepPlan(ctx, "pln_1")
	st, trades := e.PlanStats("pln_1")
	if st.Trades != 1 {
		p, _ := e.Store.Plan("pln_1")
		t.Fatalf("plan did not trade: %+v (last signal %q)", st, p.LastSignal)
	}
	tr := trades[0]
	// $8 of a $20 wallet is 40%: allowed because the owner chose the size.
	if tr.Kind != "plan" || tr.Token != "cbBTC" || !strings.EqualFold(tr.TokenAddr, cbbtcAddr) || tr.SizeUSD != 8 || tr.PlanID != "pln_1" {
		t.Fatalf("plan trade: %+v", tr)
	}
	if tr.TakeProfit < tr.Entry*1.019 || tr.StopLoss > tr.Entry*0.991 {
		t.Fatalf("targets not from the plan: entry %v tp %v sl %v", tr.Entry, tr.TakeProfit, tr.StopLoss)
	}
	e.RefreshTrade(ctx, tr.ID)
	e.RefreshTrade(ctx, tr.ID)
	cur, _ := e.Store.Trade(tr.ID)
	w, _ := e.Wallets.Get("follower1")
	if cur.Status != "filled" || cur.BracketOrderID == "" || e.Venue.Balance(w.Address(), cbbtcAddr) <= 0 {
		t.Fatalf("after fill: %s bracket=%q cbBTC=%v", cur.Status, cur.BracketOrderID, e.Venue.Balance(w.Address(), cbbtcAddr))
	}
	// While a position is open the plan waits.
	e.StepPlan(ctx, "pln_1")
	if p, _ := e.Store.Plan("pln_1"); !strings.Contains(p.LastSignal, "holding") {
		t.Fatalf("last signal %q", p.LastSignal)
	}
	// Exit now sells the cbBTC (token-aware exit).
	if _, err := e.ExitPosition(ctx, tr.ID); err != nil {
		t.Fatalf("exit: %v", err)
	}
	e.RefreshTrade(ctx, tr.ID)
	if cur, _ = e.Store.Trade(tr.ID); cur.Status != "closed" || e.Venue.Balance(w.Address(), cbbtcAddr) != 0 {
		t.Fatalf("after exit: %s", cur.Status)
	}
	// Interval: the next entry waits for its minute.
	e.StepPlan(ctx, "pln_1")
	if p, _ := e.Store.Plan("pln_1"); !strings.HasPrefix(p.LastSignal, "next entry in") {
		t.Fatalf("last signal %q", p.LastSignal)
	}
}

func TestPlanStopsAtLossLimitAndEndsOnTime(t *testing.T) {
	e, _ := planSetup(t)
	ctx := context.Background()
	e.Store.PutPlan(newPlan("pln_loss", "follower1", "interval", func(p *store.Plan) { p.LossLimitPct = 10 }))
	loss := -1.7
	e.Store.PutTrade(&store.Trade{ID: "t_loss", PlanID: "pln_loss", WalletID: "follower1", Status: "closed", PnLUSD: &loss, CreatedAt: time.Now()})
	e.StepPlan(ctx, "pln_loss")
	if p, _ := e.Store.Plan("pln_loss"); p.Status != "stopped" || !strings.Contains(p.StatusReason, "loss limit") {
		t.Fatalf("loss limit: %s %q", p.Status, p.StatusReason)
	}
	e.Store.PutPlan(newPlan("pln_end", "follower2", "interval", func(p *store.Plan) { p.EndsAt = time.Now().Add(-time.Second) }))
	e.StepPlan(ctx, "pln_end")
	if p, _ := e.Store.Plan("pln_end"); p.Status != "ended" {
		t.Fatalf("end: %s", p.Status)
	}
	if st, _ := e.PlanStats("pln_end"); st.Trades != 0 {
		t.Fatal("an ended plan traded")
	}
}

func TestPlanAutoPilotPicksTheBestStrategy(t *testing.T) {
	e, _ := planSetup(t)
	for i, v := range []float64{100, 90, 95} { // contrarian: down then partly back
		_ = i
		e.Store.AddEquity("contrarian", v)
	}
	e.Store.AddEquity("dipbuyer", 100)
	e.Store.AddEquity("dipbuyer", 112) // +12%, no drawdown: best score
	e.Store.AddEquity("trendhunter", 100)
	e.Store.AddEquity("trendhunter", 101)
	if s, why := e.bestStrategy(); s != "dip" || !strings.Contains(why, "dip") {
		t.Fatalf("auto picked %s (%s)", s, why)
	}
}

func TestPlansDoNotTradeRealMoneyUnlessAllowed(t *testing.T) {
	e, _ := setup(t) // live config (not paper, not dry)
	e.Store.PutPlan(newPlan("pln_live", "follower1", "interval", func(p *store.Plan) { p.Token = "WETH" }))
	e.StepPlan(context.Background(), "pln_live")
	if st, _ := e.PlanStats("pln_live"); st.Trades != 0 {
		t.Fatal("plan traded real money with LIVE_AGENTS off")
	}
	if p, _ := e.Store.Plan("pln_live"); !strings.Contains(p.LastSignal, "LIVE_AGENTS") {
		t.Fatalf("last signal %q", p.LastSignal)
	}
}

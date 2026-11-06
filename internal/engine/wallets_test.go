package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vikram/agent-arena/internal/store"
)

func TestInvestTradesInvestorWalletWithoutAgentMoney(t *testing.T) {
	e, ff := setup(t)
	agent, _ := e.Wallets.Get("trendhunter")
	inv, _ := e.Wallets.Get("follower1")
	ff.usdc[strings.ToLower(agent.Address())] = 0 // the agent itself has no money
	e.Store.PutFollow(&store.Follow{FollowerID: inv.ID, AgentID: "trendhunter", Mode: "invest", MaxPerTradeUSD: 3, DailyCapUSD: 6, Active: true})
	e.investAll(context.Background(), intent())
	var got *store.Trade
	for _, tr := range e.Store.Trades("trendhunter", 0) {
		if tr.WalletID == inv.ID {
			tc := tr
			got = &tc
		}
	}
	if got == nil || got.Kind != "invest" || got.SizeUSD != 3 || got.CopyOf != "" {
		t.Fatalf("invest trade: %+v", got)
	}
	if e.Store.CopySpentToday(inv.ID) != 3 {
		t.Fatalf("daily cap not reserved: %v", e.Store.CopySpentToday(inv.ID))
	}
	// Copy-mode followers are not traded on a bare signal.
	e.Store.PutFollow(&store.Follow{FollowerID: inv.ID, AgentID: "trendhunter", Mode: "copy", MaxPerTradeUSD: 3, Active: true})
	before := len(e.Store.Trades("", 0))
	e.investAll(context.Background(), intent())
	if len(e.Store.Trades("", 0)) != before {
		t.Fatal("a copy follower was traded without an agent trade")
	}
}

func TestLiveBudgetCapsRealMoney(t *testing.T) {
	e, _ := setup(t)
	e.Cfg.LiveBudgetUSD, e.Cfg.TestTradeUSD, e.Cfg.TestMaxCostPct = 5, 1, 12
	if res := e.Propose(context.Background(), intent()); res.Trade == nil { // $4
		t.Fatalf("first trade: %+v", res)
	}
	in := intent()
	in.WalletID, in.Test = "follower1", true // $1 test: total $5, allowed
	if res := e.Propose(context.Background(), in); res.Trade == nil || res.Trade.SizeUSD != 1 || res.Trade.Kind != "test" {
		t.Fatalf("test trade: %+v", res)
	}
	if e.LiveSpent() != 5 {
		t.Fatalf("live spent %v", e.LiveSpent())
	}
	in.WalletID = "trendhunter"
	if res := e.Propose(context.Background(), in); res.Decision.Code != "LIVE_BUDGET" {
		t.Fatalf("expected LIVE_BUDGET, got %+v", res.Decision)
	}
}

func TestTestTradeHasItsOwnCostCapAndNoCopies(t *testing.T) {
	e, ff := setup(t)
	ff.costFrac = 0.035 // what $1-$2 really costs on Flash
	e.Cfg.TestTradeUSD, e.Cfg.TestMaxCostPct = 1, 12
	e.Store.PutFollow(&store.Follow{FollowerID: "follower1", AgentID: "trendhunter", MaxPerTradeUSD: 2, Active: true})
	in := intent()
	in.Test = true
	res := e.Propose(context.Background(), in)
	if res.Trade == nil || res.Trade.OriginalTP != 0 || res.Trade.TakeProfit != 3090 {
		t.Fatalf("test trade: %+v", res) // no net-target bump on a test
	}
	e.Wait()
	for _, tr := range e.Store.Trades("", 0) {
		if tr.WalletID == "follower1" {
			t.Fatal("a test trade was copied to a follower")
		}
	}
	// A normal $4 trade at the same cost is refused by the regular cost guard.
	in.Test, in.WalletID = false, "follower1"
	if res := e.Propose(context.Background(), in); res.Trade != nil || !strings.Contains(res.Error, "cost guard") {
		t.Fatalf("expected cost guard: %+v", res)
	}
}

func TestPausedWalletAndLossLimit(t *testing.T) {
	e, _ := setup(t)
	e.Store.UpdateProfile("trendhunter", func(p *store.WalletProfile) { p.Paused, p.PausedReason = true, "owner asked" })
	if res := e.Propose(context.Background(), intent()); res.Decision.Code != "WALLET_PAUSED" {
		t.Fatalf("expected WALLET_PAUSED: %+v", res.Decision)
	}
	e.Store.UpdateProfile("trendhunter", func(p *store.WalletProfile) { p.Paused, p.LossLimitPct = false, 10 })
	e.Store.AddEquity("trendhunter", 100)
	e.Store.AddEquity("trendhunter", 85) // -15%
	if res := e.Propose(context.Background(), intent()); res.Decision.Code != "LOSS_LIMIT" {
		t.Fatalf("expected LOSS_LIMIT: %+v", res.Decision)
	}
	if p, _ := e.Store.Profile("trendhunter"); !p.Paused {
		t.Fatal("loss limit should pause the wallet")
	}
}

func TestEmergencyStopBlocksEverything(t *testing.T) {
	e, _ := setup(t)
	e.Halt(context.Background(), "test")
	if res := e.Propose(context.Background(), intent()); res.Decision.Code != "HALTED" {
		t.Fatalf("expected HALTED: %+v", res.Decision)
	}
	e.Resume()
	if res := e.Propose(context.Background(), intent()); res.Trade == nil {
		t.Fatalf("after resume: %+v", res)
	}
}

func TestPaperWithdrawOnlyToPayout(t *testing.T) {
	e, _, v, _ := paperSetup(t)
	e.Venue = v
	w, _ := e.Wallets.Get("follower1")
	if _, _, err := e.Withdraw(context.Background(), w.ID, "USDC", 0); !errors.Is(err, ErrNoPayout) {
		t.Fatalf("expected ErrNoPayout, got %v", err)
	}
	e.Store.SetPayout(w.ID, "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf")
	// The API path can never change it.
	e.Store.UpdateProfile(w.ID, func(p *store.WalletProfile) { p.Payout = "0x000000000000000000000000000000000000dEaD" })
	e.Store.PutProfile(store.WalletProfile{ID: w.ID, Payout: "0x000000000000000000000000000000000000dEaD"})
	if p, _ := e.Store.Profile(w.ID); p.Payout != "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf" {
		t.Fatalf("payout was changed to %s", p.Payout)
	}
	tx, amt, err := e.Withdraw(context.Background(), w.ID, "USDC", 0)
	if err != nil || amt != 20 || !strings.HasPrefix(tx, "paper:") || v.Balance(w.Address(), usdcAddr) != 0 {
		t.Fatalf("withdraw: tx=%s amt=%v err=%v left=%v", tx, amt, err, v.Balance(w.Address(), usdcAddr))
	}
}

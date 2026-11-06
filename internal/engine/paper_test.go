package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/paper"
)

// paperSetup runs the engine in paper mode: quotes come from the Flash double
// (standing in for the real API), orders and balances from the paper venue.
func paperSetup(t *testing.T) (*Engine, *fakeFlash, *paper.Venue, *atomic.Value) {
	t.Helper()
	e, ff := setup(t)
	px := &atomic.Value{}
	px.Store(3000.0)
	v, err := paper.New(paper.Options{BaseURL: e.Cfg.FlashBaseURL, APIKey: "dpka_test", FeeBps: "30", Chain: "base",
		USDC: usdcAddr, WETH: wethAddr, SeedUSDC: 20, Price: func(string) float64 { return px.Load().(float64) }, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	e.Flash.SetTransport(v)
	e.Cfg.Paper = true
	return e, ff, v, px
}

func TestPaperTradeRunsTheFullLifecycle(t *testing.T) {
	e, ff, v, px := paperSetup(t)
	ctx := context.Background()
	w, _ := e.Wallets.Get("trendhunter")

	res := e.Propose(ctx, intent())
	if res.Trade == nil || res.Error != "" {
		t.Fatalf("propose: %+v", res)
	}
	if ff.orders.Load() != 0 {
		t.Fatal("paper orders must never reach the real order endpoint")
	}
	if !res.Trade.Paper || res.Trade.DryRun {
		t.Fatalf("trade should be marked paper: %+v", res.Trade)
	}
	e.RefreshTrade(ctx, res.Trade.ID) // entry filled, bracket active
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "filled" || tr.BracketOrderID == "" || tr.WETHAmount <= 0 || tr.USDCSpent != 4 {
		t.Fatalf("after fill: status=%s bracket=%q weth=%v spent=%v", tr.Status, tr.BracketOrderID, tr.WETHAmount, tr.USDCSpent)
	}
	if got := v.Balance(w.Address(), usdcAddr); got != 16 {
		t.Fatalf("paper USDC after the $4 entry: %v", got)
	}
	// Price runs through the take profit: the bracket fires on the next tick.
	px.Store(tr.TakeProfit + 1)
	v.Tick(ctx)
	e.RefreshTrade(ctx, tr.ID)
	tr, _ = e.Store.Trade(tr.ID)
	if tr.Status != "closed" || tr.PnLUSD == nil || *tr.PnLUSD <= 0 || tr.CloseReason != "take profit hit" {
		t.Fatalf("after TP: status=%s pnl=%v reason=%q", tr.Status, tr.PnLUSD, tr.CloseReason)
	}
	if v.Balance(w.Address(), wethAddr) != 0 || v.Balance(w.Address(), usdcAddr) <= 20 {
		t.Fatalf("paper balances after the win: usdc=%v weth=%v", v.Balance(w.Address(), usdcAddr), v.Balance(w.Address(), wethAddr))
	}
}

func TestPaperExitNowAndStopLoss(t *testing.T) {
	e, _, v, px := paperSetup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	e.RefreshTrade(ctx, res.Trade.ID)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, err := e.ExitPosition(ctx, res.Trade.ID) // cancel bracket (signed) + market sell (signed)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	e.RefreshTrade(ctx, tr.ID)
	tr2, _ := e.Store.Trade(tr.ID)
	if tr2.Status != "closed" || tr2.CloseReason != "exited by operator" || tr2.PnLUSD == nil {
		t.Fatalf("after exit now: %+v", tr2)
	}
	// A second trade that hits its stop.
	in := intent()
	res = e.Propose(ctx, in)
	if res.Trade == nil {
		t.Fatalf("second propose: %+v", res)
	}
	e.RefreshTrade(ctx, res.Trade.ID)
	e.RefreshTrade(ctx, res.Trade.ID)
	cur, _ := e.Store.Trade(res.Trade.ID)
	px.Store(cur.StopLoss - 1)
	v.Tick(ctx)
	e.RefreshTrade(ctx, cur.ID)
	cur, _ = e.Store.Trade(cur.ID)
	if cur.Status != "closed" || cur.PnLUSD == nil || *cur.PnLUSD >= 0 || cur.CloseReason != "stop loss hit" {
		t.Fatalf("after SL: status=%s pnl=%v reason=%q", cur.Status, cur.PnLUSD, cur.CloseReason)
	}
}

func TestPaperVenueRejectsForgedSignatures(t *testing.T) {
	e, _, _, _ := paperSetup(t)
	ctx := context.Background()
	w, _ := e.Wallets.Get("trendhunter")
	other, _ := e.Wallets.Get("follower1")
	req := e.Exec.buyRequest(w, 4, 3090, 2940, 0)
	q, err := e.Flash.Quote(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := other.Signer.SignTypedData(q.EVM.OrderTypedData) // wrong key
	order := flash.NewOrderRequest(req, q.QuoteID, sig, q.EVM.OrderTypedData)
	if _, err := e.Flash.SubmitOrder(ctx, order); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("forged order accepted: %v", err)
	}
}

func TestPaperBatchCancel(t *testing.T) {
	e, _, _, _ := paperSetup(t)
	ctx := context.Background()
	w, _ := e.Wallets.Get("trendhunter")
	res := e.Propose(ctx, intent())
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	e.RefreshTrade(ctx, res.Trade.ID)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	ids := []string{tr.BracketOrderID, res.Trade.OrderID} // resting bracket + the filled entry
	sig, _ := w.Signer.SignPersonal(flash.BatchCancelMessage(ids))
	out, err := e.Flash.CancelBatch(ctx, ids, sig)
	if err != nil || len(out) != 2 || !out[0].OK || out[1].OK || out[1].ErrorCode != "ALREADY_FILLED" {
		t.Fatalf("batch cancel: %+v %v", out, err)
	}
	o, _ := e.Flash.GetOrder(ctx, ids[0], w.Address())
	if o.Status != flash.StatusCancelled {
		t.Fatalf("bracket after batch cancel: %s", o.Status)
	}
	// The engine notices: WETH is held with no protection.
	e.RefreshTrade(ctx, tr.ID)
	if cur, _ := e.Store.Trade(tr.ID); cur.Status != "unprotected" {
		t.Fatalf("expected unprotected, got %s", cur.Status)
	}
	// Someone else's signature cancels nothing.
	k, _, _ := evm.GenerateKey()
	d := evm.PersonalMessageHash([]byte(flash.BatchCancelMessage(ids)))
	bad, _ := k.Sign(d)
	out, _ = e.Flash.CancelBatch(ctx, ids, evm.SigHex(bad))
	for _, r := range out {
		if r.OK || r.ErrorCode != "NOT_FOUND" {
			t.Fatalf("stranger cancelled an order: %+v", out)
		}
	}
}

func TestPaperTradesCloseWhenTheLedgerWasReset(t *testing.T) {
	e, _, _, _ := paperSetup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	// A brand-new venue knows none of the old orders (as after a damaged ledger).
	fresh, _ := paper.New(paper.Options{BaseURL: e.Cfg.FlashBaseURL, APIKey: "dpka_test", Chain: "base", USDC: usdcAddr, WETH: wethAddr,
		SeedUSDC: 20, Price: func(string) float64 { return 3000 }})
	e.Flash.SetTransport(fresh)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "terminated" || !strings.Contains(tr.CloseReason, "reset") {
		t.Fatalf("orphaned paper trade: %s %q", tr.Status, tr.CloseReason)
	}
	if e.Referee.Check(intent(), 20).Code == "OPEN_POSITION" {
		t.Fatal("the closed orphan still blocks new trades")
	}
}

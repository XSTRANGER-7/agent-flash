package engine

// Advanced Flash order styles. Every style goes through the same Flash
// /quote + /order endpoints; the arena keeps its one rule on top: every
// position has a stop loss.
//
//	market  market entry + attached bracket (TP / SL)
//	limit   limit entry (limitCrossPrice) + attached bracket
//	twap    TWAP entry (DCA): the size is split into one slice a minute over
//	        TwapMinutes, with the bracket attached (it activates on slice one)
//	stop    stop buy: buys when price rises to StopPrice. Flash does not attach
//	        brackets to stop orders, so the protection (a bracket, or a
//	        trailing stop) is placed as its own Flash order right after the fill.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// Styles lists the entry styles for the UI.
var Styles = map[string]string{
	"market": "Market: buy now, take profit and stop loss attached",
	"limit":  "Limit: buy only at your price or better",
	"twap":   "DCA / TWAP: spread the buy over time, one slice a minute",
	"stop":   "Stop buy: buy when price breaks above a level",
}

func normalizeStyle(in *referee.Intent) error {
	if in.Style == "" {
		switch {
		case strings.HasPrefix(in.OrderType, "limit") || in.EntryLimit > 0:
			in.Style = "limit"
		case strings.HasPrefix(in.OrderType, "twap"):
			in.Style = "twap"
		case strings.HasPrefix(in.OrderType, "stop"):
			in.Style = "stop"
		default:
			in.Style = "market"
		}
	}
	switch in.Style {
	case "market":
		if in.OrderType == "" {
			in.OrderType = "market+bracket"
		}
	case "limit":
		if in.EntryLimit <= 0 || (in.Price > 0 && in.EntryLimit >= in.Price) {
			return errors.New("a limit buy needs a limit price below the current price")
		}
		in.OrderType = "limit+bracket"
	case "twap":
		if in.TwapMinutes < 5 || in.TwapMinutes > 7*24*60 {
			return errors.New("DCA / TWAP runs 5 minutes to 7 days")
		}
		in.OrderType = "twap+bracket"
	case "stop":
		if in.StopPrice <= 0 || (in.Price > 0 && in.StopPrice <= in.Price) {
			return errors.New("a stop buy triggers above the current price")
		}
		in.OrderType = "stop+protect"
	default:
		return fmt.Errorf("unknown order style %q", in.Style)
	}
	if in.ProtectMode == "" {
		in.ProtectMode = "bracket"
	}
	switch in.ProtectMode {
	case "bracket":
	case "trailing":
		if in.Style != "stop" {
			return errors.New("trailing protection is for stop buys; the other styles get Flash's attached bracket")
		}
		if in.TrailPct < 0.5 || in.TrailPct > 50 {
			return errors.New("trailing stop distance must be 0.5%-50%")
		}
	default:
		return fmt.Errorf("unknown protection %q", in.ProtectMode)
	}
	return nil
}

// Protect places a protective sell on a held position, as its own Flash
// order: a bracket (take profit + stop loss triggers) or a trailing stop.
func (e *Executor) Protect(ctx context.Context, w *wallet.Wallet, tok config.Token, qty, tp, sl, trailPct float64) (*Placement, error) {
	l := e.lock(w.ID)
	l.Lock()
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	no := false
	qs := fmtUnits(qty, tok.Decimals)
	req := &flash.QuoteRequest{TargetChain: e.cfg.Chain, ContraChain: e.cfg.Chain, TargetAsset: tok.Address, ContraAsset: e.cfg.USDC,
		Side: "sell", Qty: qs, FunderAddress: w.Address(), FlashIntegratorFeeBps: e.cfg.FeeBps, ForceMinimalAllowance: true, EVMUsePermit2: &no}
	what := ""
	if trailPct > 0 {
		req.OrderType = "trailing-stop"
		req.TrailingStop = &flash.TrailingStop{TrailPercent: strconv.FormatFloat(trailPct/100, 'f', -1, 64)}
		what = fmt.Sprintf("trailing stop %.1f%% below the high", trailPct)
	} else {
		req.OrderType = "bracket"
		req.Triggers = []flash.Trigger{{NotionalPrice: fmtPrice(sl), TriggerType: "lower"}, {NotionalPrice: fmtPrice(tp), TriggerType: "upper"}}
		what = fmt.Sprintf("bracket TP %s / SL %s", fmtPrice(tp), fmtPrice(sl))
	}
	p := &Placement{DryRun: e.cfg.DryRun, Paper: e.cfg.Paper, TakeProfit: tp, StopLoss: sl}
	q, err := e.quote(ctx, req)
	if err != nil {
		return nil, err
	}
	units, err := decToUnits(qs, tok.Decimals)
	if err != nil {
		return nil, err
	}
	if err := e.guardTypedData(q.EVM.OrderTypedData, w.Address(), expect{from: tok.Address, to: e.cfg.USDC, exact: units}); err != nil {
		return nil, fmt.Errorf("protection payload: %w", err)
	}
	if err := e.checkSetupTxs(q.SetupTxs, w.Address()); err != nil {
		return nil, err
	}
	if q.EVM.ApproveTx != nil {
		if _, err := e.cappedApprove(ctx, w, q.EVM.ApproveTx, tok.Address, tok.Decimals, qty*1.01); err != nil {
			return nil, fmt.Errorf("approve %s: %w", tok.Symbol, err)
		}
	}
	sig, err := w.Signer.SignTypedData(q.EVM.OrderTypedData)
	if err != nil {
		return nil, err
	}
	p.QuoteID = q.QuoteID
	if e.cfg.DryRun {
		p.OrderID = "dryrun-" + q.QuoteID
		p.step("protected", what+" signed (dry run, not sent)")
		return p, nil
	}
	order := flash.NewOrderRequest(req, q.QuoteID, sig, q.EVM.OrderTypedData)
	order.AttributionCode = e.cfg.AttributionCode
	res, err := e.flash.SubmitOrder(ctx, order)
	metrics.Inc("flash_orders_total")
	if err != nil {
		metrics.Inc("flash_order_errors_total")
		return nil, fmt.Errorf("submit protection: %w", err)
	}
	p.OrderID = res.OrderID
	p.step("protected", fmt.Sprintf("Flash %s order %s: %s", req.OrderType, res.OrderID, what))
	return p, nil
}

// protectAfterFill puts the stop loss on a filled stop buy.
func (e *Engine) protectAfterFill(tr store.Trade) {
	w, ok := e.Wallets.Get(tr.WalletID)
	if !ok || tr.WETHAmount <= 0 || tr.BracketOrderID != "" {
		return
	}
	trail := 0.0
	if tr.ProtectMode == "trailing" {
		trail = tr.TrailPct
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := e.Exec.Protect(ctx, w, e.tradeToken(tr), tr.WETHAmount, tr.TakeProfit, tr.StopLoss, trail)
	if err != nil {
		e.Log.Warn("protection after fill failed", "trade", tr.ID, "err", err)
		e.markUnprotected(tr, "", "placing its stop loss failed ("+shortErr(err)+")")
		return
	}
	t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.BracketOrderID, x.Protection = p.OrderID, "active"
		x.Timeline = append(x.Timeline, p.Steps...)
	})
	if ok {
		e.Feed.Publish(feed.Post{Kind: "trade_filled", AgentID: tr.AgentID, TradeID: tr.ID, Text: fmt.Sprintf("%s: stop buy filled; protection placed (%s)", e.label(*t), p.Steps[len(p.Steps)-1].Detail)})
	}
}

// StyleName is the short word for a style ("DCA" for twap).
func StyleName(s string) string {
	switch s {
	case "twap":
		return "DCA"
	case "stop":
		return "stop buy"
	case "limit":
		return "limit"
	}
	return "market"
}

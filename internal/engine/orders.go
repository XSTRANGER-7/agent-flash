package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/outbox"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// Order lifecycle on Flash (attached bracket):
//
//	entry PENDING/ACCEPTED -> FILLED            trade "placed" -> "filled"
//	attachedBracket pending_activation -> active (bracketOrderId appears on the first fill)
//	bracket order FILLED (TP or SL sold)        trade "closed" with PnL from fills
//	bracket CANCELLED/REJECTED without selling  trade "unprotected" (WETH still in the wallet)
//	operator "exit now": cancel bracket + market sell -> "closed"
//
// The Flash orders WebSocket triggers an immediate refresh; REST polling is
// the fallback (every ORDER_POLL while the stream is down, 30s while live).

// OnStreamOrders is the Flash stream callback (never blocks the stream).
func (e *Engine) OnStreamOrders(orders []flash.Order, _ bool) {
	for _, o := range orders {
		for _, id := range []string{o.OrderID, o.SourceEntryOrderID} {
			if id == "" {
				continue
			}
			select {
			case e.refreshCh() <- id:
			default: // a full queue just means the next poll catches it
			}
		}
	}
}

func (e *Engine) refreshCh() chan string {
	e.refreshOnce.Do(func() { e.refresh = make(chan string, 256) })
	return e.refresh
}

// RestoreOpen re-registers open positions with the referee after a restart,
// so the one-position-per-wallet rule survives restarts.
func (e *Engine) RestoreOpen() {
	for _, t := range e.Store.OpenTrades() {
		if !t.DryRun {
			e.Referee.MarkOpen(t.WalletID)
		}
	}
}

// StreamState is "live", "connecting", "off" (stream disabled or stopped).
func (e *Engine) StreamState() string {
	if e.Stream == nil {
		return "off"
	}
	return e.Stream.State()
}

// WatchOrders follows every open trade until its position is sold.
func (e *Engine) WatchOrders(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	held := map[string]bool{}
	var last time.Time
	ch := e.refreshCh()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			if tr, ok := e.tradeByOrder(id); ok {
				e.refreshTrade(ctx, tr, held)
			}
			continue
		case <-t.C:
		}
		interval := e.Cfg.OrderPoll
		if e.Stream != nil && e.Stream.Live() && !e.Cfg.Paper {
			// The orders stream triggers refreshes; polling is the backstop.
			// (Paper orders live in the paper venue, never on the stream.)
			interval = 30 * time.Second
		}
		if time.Since(last) < interval {
			continue
		}
		last = time.Now()
		for _, tr := range e.Store.OpenTrades() {
			if !tr.DryRun {
				e.refreshTrade(ctx, tr, held)
			}
		}
	}
}

func (e *Engine) tradeByOrder(id string) (store.Trade, bool) {
	for _, t := range e.Store.OpenTrades() {
		if !t.DryRun && (t.OrderID == id || t.BracketOrderID == id || t.ExitOrderID == id) {
			return t, true
		}
	}
	return store.Trade{}, false
}

// RefreshTrade re-reads one trade's orders now (used by the API after actions).
func (e *Engine) RefreshTrade(ctx context.Context, id string) {
	if t, ok := e.Store.Trade(id); ok && t.Open() && !t.DryRun {
		e.refreshTrade(ctx, *t, map[string]bool{})
	}
}

func (e *Engine) refreshTrade(ctx context.Context, tr store.Trade, held map[string]bool) {
	w, ok := e.Wallets.Get(tr.WalletID)
	if !ok {
		return
	}
	switch {
	case tr.ExitOrderID != "":
		e.followExit(ctx, w, tr)
	case tr.Status == "placed":
		e.followEntry(ctx, w, tr)
	case tr.Status == "filled" || tr.Status == "unprotected":
		e.followBracket(ctx, w, tr, held)
	}
}

func (e *Engine) followEntry(ctx context.Context, w *wallet.Wallet, tr store.Trade) {
	o, err := e.Flash.GetOrder(ctx, tr.OrderID, w.Address())
	if err != nil {
		if e.paperGone(tr, err) {
			return
		}
		e.Log.Debug("get entry order", "trade", tr.ID, "err", err)
		return
	}
	switch {
	case o.Status == flash.StatusFilled || (o.Terminal() && o.FilledTarget() > 0):
		e.markFilled(tr, o)
	case o.Terminal():
		status := strings.ToLower(strings.TrimPrefix(o.Status, "ORDER_STATUS_"))
		e.closeTrade(tr, status, HumanReason(o.Reason(), "entry "+status), o.Reason(), nil, nil)
	default:
		if ab := o.AttachedBracket; ab != nil && ab.BracketOrderID != "" && tr.BracketOrderID == "" {
			e.Store.UpdateTrade(tr.ID, func(x *store.Trade) { x.BracketOrderID, x.Protection = ab.BracketOrderID, "active" })
		}
	}
}

func protectionOf(ab *flash.OrderBracket) (string, string) {
	if ab == nil {
		return "pending", ""
	}
	switch {
	case ab.BracketOrderID != "" || ab.Status == "active":
		return "active", ab.BracketOrderID
	case ab.Status == "never_activated":
		return "none", ""
	}
	return "pending", ""
}

func (e *Engine) markFilled(tr store.Trade, o *flash.Order) {
	weth, spent := o.FilledTarget(), o.FilledContra()
	if spent <= 0 {
		spent = tr.SizeUSD
	}
	fill := o.AvgNotionalPrice()
	be := 0.0
	if weth > 0 {
		// All-in entry price, plus an exit that costs about what the entry did.
		be = round2(spent / weth * (1 + tr.EntryCostPct/100))
	}
	prot, bid := protectionOf(o.AttachedBracket)
	tx := o.TxHash()
	t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.Status, x.FillPrice, x.WETHAmount, x.USDCSpent, x.EntryTx = "filled", fill, weth, spent, tx
		x.BreakEvenPrice, x.BracketOrderID, x.Protection = be, bid, prot
		d := fmt.Sprintf("bought %.8f %s for $%.2f at $%.2f", weth, tokenSym(tr), spent, fill)
		if tx != "" {
			d += " (tx " + tx + ")"
		}
		x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "filled", Detail: d})
	})
	if !ok {
		return
	}
	e.invalidate(tr.WalletID)
	if t.Style == "stop" {
		e.protectAfterFill(*t) // stop orders carry no attached bracket
		return
	}
	e.Feed.Publish(feed.Post{Kind: "trade_filled", AgentID: tr.AgentID, TradeID: tr.ID,
		Text: fmt.Sprintf("%s filled at $%.2f; bracket %s (TP %.2f / SL %.2f, break-even %.2f)", e.label(*t), fill, prot, t.TakeProfit, t.StopLoss, be)})
}

func (e *Engine) followBracket(ctx context.Context, w *wallet.Wallet, tr store.Trade, held map[string]bool) {
	if tr.BracketOrderID == "" && tr.Style == "stop" {
		if tr.Status == "filled" {
			e.protectAfterFill(tr) // retry: the stop buy filled but its protection is not placed yet
		}
		return
	}
	if tr.BracketOrderID == "" {
		o, err := e.Flash.GetOrder(ctx, tr.OrderID, w.Address())
		if err != nil {
			return
		}
		prot, bid := protectionOf(o.AttachedBracket)
		switch {
		case bid != "":
			if t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) { x.BracketOrderID, x.Protection = bid, "active" }); ok {
				tr = *t
			}
		case prot == "none" && tr.Status != "unprotected":
			e.markUnprotected(tr, o.Reason(), "the bracket never activated")
			return
		case o.AttachedBracket == nil:
			// Flash did not report a bracket at all: fall back to watching the balance.
			e.balanceFallback(ctx, w, tr, held)
			return
		default:
			return // pending activation
		}
	}
	b, err := e.Flash.GetOrder(ctx, tr.BracketOrderID, w.Address())
	if err != nil {
		e.paperGone(tr, err)
		return
	}
	if tr.TPOrderID != "" { // separate mode: a standalone take-profit next to the stop-loss
		if tpo, terr := e.Flash.GetOrder(ctx, tr.TPOrderID, w.Address()); terr == nil && tpo.Status == flash.StatusFilled {
			e.cancelQuietly(ctx, w, tr.BracketOrderID) // the stop loss has nothing left to protect
			e.closeFromExit(ctx, w, tr, tpo, false)
			return
		}
	}
	switch {
	case b.Status == flash.StatusFilled:
		if tr.TPOrderID != "" {
			e.cancelQuietly(ctx, w, tr.TPOrderID)
		}
		e.closeFromExit(ctx, w, tr, b, false)
	case b.Terminal():
		if tr.Status != "unprotected" {
			e.markUnprotected(tr, b.Reason(), "bracket "+strings.ToLower(strings.TrimPrefix(b.Status, "ORDER_STATUS_")))
		}
	default:
		if tr.Protection != "active" {
			e.Store.UpdateTrade(tr.ID, func(x *store.Trade) { x.Protection = "active" })
		}
		e.maybeMoveStop(ctx, w, tr)
	}
}

func (e *Engine) markUnprotected(tr store.Trade, code, what string) {
	reason := HumanReason(code, what)
	t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.Status, x.Protection, x.CloseCode = "unprotected", "none", code
		x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "unprotected", Detail: reason + "; WETH is still in the wallet, use Exit now"})
	})
	if !ok {
		return
	}
	e.Feed.Publish(feed.Post{Kind: "trade_blocked", AgentID: tr.AgentID, TradeID: tr.ID,
		Text: fmt.Sprintf("%s: %s. The position is no longer protected; the operator can exit it.", e.label(*t), reason)})
}

func (e *Engine) followExit(ctx context.Context, w *wallet.Wallet, tr store.Trade) {
	o, err := e.Flash.GetOrder(ctx, tr.ExitOrderID, w.Address())
	if err != nil {
		return
	}
	switch {
	case o.Status == flash.StatusFilled:
		e.closeFromExit(ctx, w, tr, o, true)
	case o.Terminal():
		reason := HumanReason(o.Reason(), "exit order "+strings.ToLower(strings.TrimPrefix(o.Status, "ORDER_STATUS_")))
		e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
			x.ExitOrderID, x.Status, x.Protection, x.CloseCode = "", "unprotected", "none", o.Reason()
			x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "exit_failed", Detail: reason})
		})
		e.Feed.Publish(feed.Post{Kind: "trade_blocked", AgentID: tr.AgentID, TradeID: tr.ID, Text: "Exit order failed: " + reason})
	}
}

// closeFromExit closes a trade from the order that sold the WETH. PnL comes
// from Flash's fills (USDC received - USDC spent); the wallet balance is the
// fallback and is logged next to it for reconciliation.
func (e *Engine) closeFromExit(ctx context.Context, w *wallet.Wallet, tr store.Trade, o *flash.Order, operator bool) {
	recv, exitPx := o.FilledContra(), o.AvgNotionalPrice()
	e.invalidate(w.ID)
	usdcNow, _, balErr := e.Balances(ctx, w, 0)
	var pnl *float64
	if recv > 0 && tr.USDCSpent > 0 {
		v := round2(recv - tr.USDCSpent)
		pnl = &v
	} else if balErr == nil {
		v := round2(usdcNow - tr.USDCBefore)
		pnl = &v
	}
	if balErr == nil {
		e.Log.Info("trade closed", "trade", tr.ID, "pnlFromFills", recv-tr.USDCSpent, "pnlFromBalance", usdcNow-tr.USDCBefore)
	}
	reason := "exited by operator"
	if tr.ExitWhy != "" {
		reason = tr.ExitWhy // the arena closed it itself (time limit)
	}
	if !operator && tr.ProtectMode == "trailing" {
		reason = "trailing stop hit"
	} else if !operator {
		// Flash's fill price is all-in (after its sell cost), so a take
		// profit can fill below the trigger. A fresh fill is judged by the
		// market price now; a late one (after a restart) by the fill price
		// with the cost added back, since today's price says nothing about it.
		addr := tr.TokenAddr
		if addr == "" {
			addr = e.Cfg.WETH
		}
		ref := 0.0
		if at := lastFill(o); !at.IsZero() && time.Since(at) < 3*time.Minute {
			ref = e.tokenPrice(addr)
		}
		if ref <= 0 {
			ref = exitPx
			if tr.EntryCostPct > 0 && tr.EntryCostPct < 10 {
				ref = exitPx * (1 + tr.EntryCostPct/100)
			}
		}
		switch {
		case ref > 0 && tr.TakeProfit > 0 && ref >= tr.TakeProfit*0.997:
			reason = "take profit hit"
		case tr.StopMoved && ref > 0 && ref <= tr.StopLoss*1.003:
			reason = "stop hit after it was moved"
		case ref > 0 && ref <= tr.StopLoss*1.003:
			reason = "stop loss hit"
		case pnl != nil && *pnl > 0:
			reason = "take profit hit"
		default:
			reason = "stop loss hit"
		}
	}
	tx := o.TxHash()
	e.closeTrade(tr, "closed", reason, o.Reason(), pnl, func(x *store.Trade) {
		x.ExitPrice, x.USDCReceived, x.ExitTx, x.Protection = exitPx, recv, tx, "none"
	})
}

// balanceFallback is the pre-bracket-id logic: close only after the WETH was
// seen in the wallet and is gone again (or long after the fill).
func (e *Engine) balanceFallback(ctx context.Context, w *wallet.Wallet, tr store.Trade, held map[string]bool) {
	usdc, _, err := e.Balances(ctx, w, e.Cfg.OrderPoll/2)
	if err != nil {
		return
	}
	weth, _ := e.TokenBalance(ctx, w, tr.TokenAddr, e.Cfg.OrderPoll/2)
	if weth >= 1e-7 {
		held[tr.ID] = true
		return
	}
	if !held[tr.ID] && time.Since(tr.UpdatedAt) < 10*time.Minute {
		return // fill not visible in balances yet
	}
	delete(held, tr.ID)
	pnl := round2(usdc - tr.USDCBefore)
	reason := "bracket exited flat"
	switch {
	case pnl > 0:
		reason = "take profit hit"
	case pnl < 0:
		reason = "stop loss hit"
	}
	e.closeTrade(tr, "closed", reason, "", &pnl, nil)
}

// maybeMoveStop moves the stop to break-even once price is halfway from
// break-even to the take profit (Flash PATCH /orders/{id}, both legs resent).
func (e *Engine) maybeMoveStop(ctx context.Context, w *wallet.Wallet, tr store.Trade) {
	if tr.ProtectMode == "trailing" || tr.ProtectMode == "separate" {
		return // a trailing stop moves itself; a lone stop-loss has no bracket legs to PATCH
	}
	on := e.Cfg.BreakEvenStop
	if tr.BreakEven != "" {
		on = tr.BreakEven == "on" // the agent maker's own setting
	}
	if !on || tr.StopMoved || tr.BracketOrderID == "" || tr.BreakEvenPrice <= 0 || tr.StopLoss >= tr.BreakEvenPrice || tr.TakeProfit <= tr.BreakEvenPrice {
		return
	}
	addr := tr.TokenAddr
	if addr == "" {
		addr = e.Cfg.WETH
	}
	px := e.tokenPrice(addr) // the coin this trade holds, not always WETH
	trigger := tr.BreakEvenPrice + (tr.TakeProfit-tr.BreakEvenPrice)/2
	if px < trigger {
		return
	}
	e.mu.Lock()
	if e.stopTries == nil {
		e.stopTries = map[string]int{}
	}
	tries := e.stopTries[tr.ID]
	e.stopTries[tr.ID] = tries + 1
	e.mu.Unlock()
	if tries >= 3 {
		return
	}
	newSL := round2(tr.BreakEvenPrice)
	if err := e.Exec.MoveStop(ctx, w, tr.BracketOrderID, newSL, tr.TakeProfit); err != nil {
		e.Log.Warn("move stop to break-even failed", "trade", tr.ID, "err", err)
		if tries == 2 {
			e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
				x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "stop_move_failed", Detail: shortErr(err)})
			})
		}
		return
	}
	old := tr.StopLoss
	t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.StopLoss, x.StopMoved = newSL, true
		x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "stop_moved", Detail: fmt.Sprintf("stop loss moved $%.2f -> $%.2f (break-even) via Flash order update", old, newSL)})
	})
	if ok {
		e.Feed.Publish(feed.Post{Kind: "trade_filled", AgentID: tr.AgentID, TradeID: tr.ID,
			Text: fmt.Sprintf("%s: price reached $%.2f, stop moved to break-even $%.2f. This trade can no longer lose much.", e.label(*t), px, newSL)})
	}
}

// ErrNotExitable is returned when a trade has no position to sell.
var ErrNotExitable = errors.New("trade has no open position to exit")

// ErrTradeNotFound is returned for an unknown trade id.
var ErrTradeNotFound = errors.New("trade not found")

// ExitPosition cancels the live bracket (if any) and sells the wallet's WETH
// at market. The result is tracked like any other order.
func (e *Engine) ExitPosition(ctx context.Context, id string) (*store.Trade, error) {
	tr, ok := e.Store.Trade(id)
	if !ok {
		return nil, errors.New("trade not found")
	}
	if tr.DryRun || (tr.Status != "filled" && tr.Status != "unprotected") {
		return nil, ErrNotExitable
	}
	if tr.ExitOrderID != "" {
		return nil, errors.New("an exit order is already working")
	}
	w, ok := e.Wallets.Get(tr.WalletID)
	if !ok {
		return nil, errors.New("wallet missing")
	}
	if tr.TPOrderID != "" && tr.Protection != "none" {
		e.cancelQuietly(ctx, w, tr.TPOrderID) // separate mode: drop the take-profit first
	}
	if tr.BracketOrderID != "" && tr.Protection != "none" {
		if err := e.Exec.Cancel(ctx, w, tr.BracketOrderID); err != nil {
			var ae *flash.APIError
			if errors.As(err, &ae) && ae.HTTPStatus == 422 {
				e.RefreshTrade(ctx, id) // the bracket executed in the meantime
				return nil, errors.New("the bracket already executed; the trade is closing on its own")
			}
			return nil, fmt.Errorf("cancel bracket: %w", err)
		}
		e.Store.UpdateTrade(id, func(x *store.Trade) {
			x.Protection = "none"
			x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "bracket_cancelled", Detail: "bracket " + tr.BracketOrderID + " cancelled for an operator exit"})
		})
	}
	e.invalidate(w.ID)
	weth, err := e.TokenBalance(ctx, w, tr.TokenAddr, 0)
	if err != nil {
		return nil, fmt.Errorf("balances: %w", err)
	}
	if weth < 1e-12 {
		return nil, fmt.Errorf("the wallet holds no %s (the bracket may have sold it already)", tokenSym(*tr))
	}
	if tr.WETHAmount > 0 {
		weth = math.Min(weth, tr.WETHAmount*1.001) // never sell more than this trade bought
	}
	p, err := e.Exec.SellToken(ctx, w, e.tradeToken(*tr), weth, e.tokenPrice(tr.TokenAddr))
	if err != nil {
		e.Store.UpdateTrade(id, func(x *store.Trade) {
			x.Status = "unprotected"
			x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "exit_failed", Detail: shortErr(err)})
		})
		return nil, err
	}
	t, _ := e.Store.UpdateTrade(id, func(x *store.Trade) {
		x.ExitOrderID = p.OrderID
		x.Timeline = append(x.Timeline, p.Steps...)
	})
	e.Feed.Publish(feed.Post{Kind: "trade_placed", AgentID: tr.AgentID, TradeID: id, Text: fmt.Sprintf("Operator is exiting %s: market sell of %.8f %s", e.label(*tr), weth, tokenSym(*tr))})
	select {
	case e.refreshCh() <- p.OrderID:
	default:
	}
	return t, nil
}

func (e *Engine) closeTrade(tr store.Trade, status, reason, code string, pnl *float64, extra func(*store.Trade)) {
	t, ok := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.Status, x.CloseReason, x.PnLUSD = status, reason, pnl
		if code != "" {
			x.CloseCode = code
		}
		if extra != nil {
			extra(x)
		}
		x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "closed", Detail: reason + pnlText(pnl)})
	})
	if !ok {
		return
	}
	e.Referee.Closed(tr.WalletID)
	e.invalidate(tr.WalletID)
	e.Feed.Publish(feed.Post{Kind: "trade_closed", AgentID: tr.AgentID, TradeID: tr.ID, Text: fmt.Sprintf("%s closed (%s)%s", e.label(*t), reason, pnlText(pnl))})
	e.Outbox.Publish(outbox.Event{ID: "trade-result:" + tr.ID, Kind: "trade_result", TTLHours: 24 * 365,
		Attributes: map[string]any{"type": "trade_result", "season": e.Cfg.SeasonID, "agentid": t.AgentID, "tradeid": t.ID, "status": status},
		Payload:    t})
}

// label is "TrendHunter's trade" or "follower1's copy of TrendHunter".
func (e *Engine) label(t store.Trade) string {
	if t.CopyOf != "" {
		return fmt.Sprintf("%s's copy of %s", t.WalletID, e.name(t.AgentID))
	}
	return e.name(t.AgentID) + "'s trade"
}

func pnlText(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf(", PnL $%+.2f", *p)
}

// HumanReason turns Flash REASON_* codes into plain words.
func HumanReason(code, fallback string) string {
	m := map[string]string{
		"REASON_USER_REQUESTED":               "cancelled on request",
		"REASON_FULLY_FILLED":                 "fully filled",
		"REASON_INSUFFICIENT_FUNDER_BALANCE":  "wallet balance too low",
		"REASON_INSUFFICIENT_ASSET_BALANCE":   "not enough of the asset in the wallet",
		"REASON_MAXIMUM_SLIPPAGE_EXCEEDED":    "price moved more than the slippage limit",
		"REASON_EXECUTION_COST_EXCEEDS_LIMIT": "execution cost above the limit",
		"REASON_ORDER_REJECTED_AT_VENUE":      "rejected by the venue",
		"REASON_ORDER_TRIGGERED_ON_ENTRY":     "a leg was already triggered at entry",
		"REASON_ORDER_EXPIRED":                "expired",
		"REASON_PRICE_IMPACT_TOO_HIGH":        "price impact too high",
		"REASON_INSUFFICIENT_LIQUIDITY":       "not enough liquidity",
		"REASON_ORDER_CANNOT_BE_PLACED":       "order could not be placed",
	}
	if s, ok := m[code]; ok {
		return s
	}
	if code != "" {
		return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(code, "REASON_"), "_", " "))
	}
	return fallback
}

// paperGone closes a paper trade whose orders no longer exist in the paper
// ledger (it was reset after damage). Real orders are never closed this way.
func (e *Engine) paperGone(tr store.Trade, err error) bool {
	var ae *flash.APIError
	if !tr.Paper || !errors.As(err, &ae) || ae.HTTPStatus != 404 {
		return false
	}
	e.closeTrade(tr, "terminated", "paper ledger was reset; this paper trade no longer exists", "", nil, nil)
	return true
}

// cancelQuietly cancels a sibling order that can no longer fill usefully.
// Errors are fine: Flash also cancels it once the funder lacks the balance.
func (e *Engine) cancelQuietly(ctx context.Context, w *wallet.Wallet, orderID string) {
	if orderID == "" {
		return
	}
	if err := e.Exec.Cancel(ctx, w, orderID); err != nil {
		e.Log.Debug("sibling cancel", "order", orderID, "err", err)
	}
}

// lastFill is when an order last filled (zero if unknown).
func lastFill(o *flash.Order) time.Time {
	var at time.Time
	for _, f := range o.Fills {
		if t, err := time.Parse(time.RFC3339, f.FilledAt); err == nil && t.After(at) {
			at = t
		}
	}
	if at.IsZero() && o.ClosedAt != "" {
		if t, err := time.Parse(time.RFC3339, o.ClosedAt); err == nil {
			at = t
		}
	}
	return at
}

// Rebracket changes the take profit and stop loss of a live trade (Flash
// PATCH on its bracket, signed by the trade's wallet). The stop stays below
// the price now and within the arena's maximum stop distance; the take profit
// stays above it.
func (e *Engine) Rebracket(ctx context.Context, id string, tp, sl float64) (*store.Trade, error) {
	tr, ok := e.Store.Trade(id)
	if !ok {
		return nil, ErrTradeNotFound
	}
	if tr.Status != "filled" || tr.BracketOrderID == "" || tr.ProtectMode == "trailing" || tr.ProtectMode == "separate" {
		return nil, fmt.Errorf("%w: this trade has no live bracket to change", ErrNotExitable)
	}
	w, ok := e.Wallets.Get(tr.WalletID)
	if !ok {
		return nil, fmt.Errorf("wallet %s not found", tr.WalletID)
	}
	addr := tr.TokenAddr
	if addr == "" {
		addr = e.Cfg.WETH
	}
	px := e.tokenPrice(addr)
	switch {
	case px <= 0:
		return nil, errors.New("no live price yet")
	case sl <= 0 || sl >= px || tp <= px:
		return nil, fmt.Errorf("the stop must be below and the take profit above the price now ($%.2f)", px)
	case (px-sl)/px*100 > 5:
		return nil, errors.New("the stop may be at most 5% below the price")
	case (tp-px)/px*100 > 20:
		return nil, errors.New("the take profit may be at most 20% above the price")
	}
	tp, sl = round2(tp), round2(sl)
	if err := e.Exec.MoveStop(ctx, w, tr.BracketOrderID, sl, tp); err != nil {
		return nil, err
	}
	old := *tr
	t, _ := e.Store.UpdateTrade(tr.ID, func(x *store.Trade) {
		x.TakeProfit, x.StopLoss, x.StopMoved = tp, sl, true
		x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "bracket_moved",
			Detail: fmt.Sprintf("take profit %.2f -> %.2f, stop loss %.2f -> %.2f (Flash PATCH, signed)", old.TakeProfit, tp, old.StopLoss, sl)})
	})
	_ = e.Store.Save()
	e.Feed.Publish(feed.Post{Kind: "trade_placed", AgentID: tr.AgentID, TradeID: tr.ID,
		Text: fmt.Sprintf("%s: bracket moved to take profit %.2f / stop loss %.2f", e.label(*tr), tp, sl)})
	return t, nil
}

package engine

// Sub-wallet controls: pause / loss limit, the live budget, the emergency
// stop, withdrawals (only ever to the owner's payout address) and allowance
// revokes. These are the "spend permission" rules of Level 1 wallets,
// enforced by this service before anything is signed; the capped ERC-20
// approval is the onchain part.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

func deny(code, reason string) referee.Decision {
	return referee.Decision{OK: false, Code: code, Reason: reason}
}

// blocked records a refused intent: metrics always, the feed once per
// wallet + code per hour (see shouldPostBlock).
func (e *Engine) blocked(in referee.Intent, d referee.Decision) Result {
	metrics.Inc("referee_blocks_total")
	kind := "trade_blocked"
	if in.CopyOf != "" || in.Kind == "invest" {
		kind = "copy_blocked"
	}
	if e.shouldPostBlock(in.WalletID, d.Code, time.Now()) {
		who := e.who(in)
		e.Feed.Publish(feed.Post{Kind: kind, AgentID: in.AgentID, Text: fmt.Sprintf("Referee blocked %s: %s", who, d.Reason), Data: map[string]any{"code": d.Code, "wallet": in.WalletID}})
	} else {
		e.Log.Debug("repeat referee block not posted", "wallet", in.WalletID, "code", d.Code)
	}
	return Result{Decision: d}
}

func tradeKind(in referee.Intent, w *wallet.Wallet) string {
	switch {
	case w.Role == wallet.RoleSelf:
		return "self"
	case in.Test:
		return "test"
	case in.CopyOf != "":
		return "copy"
	case in.PlanID != "":
		return "plan"
	case in.Kind == "invest":
		return "invest"
	case w.ID == in.AgentID:
		return "agent"
	}
	return "manual"
}

// walletGate: emergency stop, a paused wallet, or a wallet past its loss limit.
func (e *Engine) walletGate(walletID string) referee.Decision {
	if on, why := e.Halted(); on {
		return deny("HALTED", "emergency stop is on ("+why+"); resume trading on the operator page")
	}
	p, ok := e.Store.Profile(walletID)
	if !ok {
		return referee.Decision{OK: true}
	}
	if p.Paused {
		return deny("WALLET_PAUSED", "wallet is paused: "+p.PausedReason)
	}
	if p.LossLimitPct > 0 {
		if dd := leaderboard.CurrentDrawdown(e.Store.PerfEquity(walletID)); dd > p.LossLimitPct {
			why := fmt.Sprintf("down %.1f%% from its peak, past its %.0f%% loss limit", dd, p.LossLimitPct)
			e.Store.UpdateProfile(walletID, func(x *store.WalletProfile) { x.Paused, x.PausedReason = true, why })
			e.Feed.Publish(feed.Post{Kind: "trade_blocked", Text: fmt.Sprintf("Wallet %s paused itself: %s. Its owner can resume it.", walletID, why)})
			return deny("LOSS_LIMIT", why)
		}
	}
	return referee.Decision{OK: true}
}

// LiveSpent is the USDC put into live (real money) entries so far. Entries
// that never filled are not counted.
func (e *Engine) LiveSpent() float64 {
	sum := 0.0
	for _, t := range e.Store.Trades("", 0) {
		if t.DryRun || t.Paper || t.Kind == "self" {
			continue // people's own wallets are their money, not the arena's budget
		}
		if t.Open() || t.USDCSpent > 0 || t.Status == "closed" {
			sum += t.SizeUSD
		}
	}
	return math.Round(sum*100) / 100
}

func (e *Engine) budgetGate(usd float64) referee.Decision {
	if !e.Cfg.Live() || e.Cfg.LiveBudgetUSD <= 0 {
		return referee.Decision{OK: true}
	}
	if used := e.LiveSpent(); used+usd > e.Cfg.LiveBudgetUSD+1e-9 {
		return deny("LIVE_BUDGET", fmt.Sprintf("live budget used $%.2f of $%.2f; raise LIVE_BUDGET_USD to allow more real-money trades", used, e.Cfg.LiveBudgetUSD))
	}
	return referee.Decision{OK: true}
}

// Halt is the emergency stop: no new orders from anyone, and every entry
// that has not filled yet is cancelled at Flash (batch cancel, one signature
// per wallet). Protective brackets stay: they guard positions already held.
func (e *Engine) Halt(ctx context.Context, reason string) (cancelled int, errs []string) {
	e.haltMu.Lock()
	e.halted, e.haltReason = true, reason
	e.haltMu.Unlock()
	e.Feed.Publish(feed.Post{Kind: "trade_blocked", Text: "Emergency stop: trading halted (" + reason + "). Open positions keep their stop loss."})
	cancelled, errs = e.CancelPending(ctx)
	e.Log.Warn("emergency stop", "reason", reason, "cancelled", cancelled, "errors", len(errs))
	return
}

func (e *Engine) Resume() {
	e.haltMu.Lock()
	e.halted, e.haltReason = false, ""
	e.haltMu.Unlock()
	e.Feed.Publish(feed.Post{Kind: "follow", Text: "Trading resumed by the operator."})
}

func (e *Engine) Halted() (bool, string) {
	e.haltMu.Lock()
	defer e.haltMu.Unlock()
	return e.halted, e.haltReason
}

// CancelPending batch-cancels every unfilled entry, grouped by wallet
// (POST /orders/cancel takes up to 25 ids signed once by their funder).
func (e *Engine) CancelPending(ctx context.Context) (int, []string) {
	byWallet := map[string][]store.Trade{}
	for _, t := range e.Store.OpenTrades() {
		if t.Status == "placed" && !strings.HasPrefix(t.OrderID, "dryrun-") {
			byWallet[t.WalletID] = append(byWallet[t.WalletID], t)
		}
	}
	n := 0
	var errs []string
	for wid, trades := range byWallet {
		w, ok := e.Wallets.Get(wid)
		if !ok {
			continue
		}
		for i := 0; i < len(trades); i += 25 {
			chunk := trades[i:min(i+25, len(trades))]
			ids := make([]string, len(chunk))
			for j, t := range chunk {
				ids[j] = strings.ToLower(t.OrderID)
			}
			res, err := e.Exec.CancelBatch(ctx, w, ids)
			if err != nil {
				errs = append(errs, wid+": "+err.Error())
				continue
			}
			for _, r := range res {
				if r.OK {
					n++
				} else if r.ErrorCode != "" {
					errs = append(errs, r.OrderID+": "+r.ErrorCode)
				}
			}
			for _, t := range chunk {
				e.RefreshTrade(ctx, t.ID) // a fill that raced the cancel is kept
				if cur, ok := e.Store.Trade(t.ID); ok && cur.Status == "placed" {
					e.closeTrade(*cur, "cancelled", "cancelled by the emergency stop", "REASON_USER_REQUESTED", nil, nil)
				}
			}
		}
	}
	return n, errs
}

// ErrNoPayout: withdrawals need the owner's address, set once.
var ErrNoPayout = errors.New("this wallet has no payout address; set it on the server with `arena payout -wallet <id> -to 0x...`")

// Withdraw sends a token from a sub-wallet to its owner's payout address,
// the only destination this service will ever send to. amount <= 0 = all.
func (e *Engine) Withdraw(ctx context.Context, walletID, token string, amount float64) (string, float64, error) {
	w, ok := e.Wallets.Get(walletID)
	if !ok {
		return "", 0, errors.New("unknown wallet")
	}
	p, _ := e.Store.Profile(walletID)
	if p.Payout == "" {
		return "", 0, ErrNoPayout
	}
	for _, t := range e.Store.OpenTrades() {
		if t.WalletID == walletID {
			return "", 0, fmt.Errorf("wallet has an open trade (%s); exit it first", t.ID)
		}
	}
	addr, dec := e.Cfg.USDC, 6
	switch strings.ToUpper(token) {
	case "USDC":
	case "WETH":
		addr, dec = e.Cfg.WETH, 18
	default:
		return "", 0, errors.New("token must be USDC or WETH")
	}
	defer e.invalidate(walletID)
	switch {
	case e.Cfg.Paper:
		have := e.Venue.Balance(w.Address(), addr)
		if amount <= 0 || amount > have {
			amount = have
		}
		if amount <= 0 {
			return "", 0, errors.New("nothing to withdraw")
		}
		tx, err := e.Venue.Withdraw(w.Address(), addr, amount)
		if err == nil {
			e.Store.AddFlow(walletID, -amount*e.tokenPriceOrOne(addr))
			e.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("[paper] %s withdrew %s %s to its owner", walletID, fmtAmount(amount), strings.ToUpper(token))})
		}
		return tx, amount, err
	case e.Cfg.DryRun:
		return "", 0, fmt.Errorf("dry run: would send %s to %s (switch MODE to live to move real money)", strings.ToUpper(token), p.Payout)
	}
	units, err := e.Exec.TokenUnits(ctx, addr, w.Address())
	if err != nil {
		return "", 0, err
	}
	if amount > 0 {
		want := new(big.Float).Mul(big.NewFloat(amount), new(big.Float).SetFloat64(math.Pow10(dec)))
		wi, _ := want.Int(nil)
		if wi.Cmp(units) < 0 {
			units = wi
		}
	}
	if units.Sign() == 0 {
		return "", 0, errors.New("nothing to withdraw")
	}
	sent, _ := new(big.Float).Quo(new(big.Float).SetInt(units), new(big.Float).SetFloat64(math.Pow10(dec))).Float64()
	tx, err := e.Exec.Transfer(ctx, w, addr, p.Payout, units)
	if err != nil {
		return tx, 0, err
	}
	e.Store.AddFlow(walletID, -sent*e.tokenPriceOrOne(addr))
	e.Log.Info("withdrawal sent", "wallet", walletID, "token", token, "amount", sent, "to", p.Payout, "tx", tx)
	e.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%s withdrew %s %s to its owner", walletID, fmtAmount(sent), strings.ToUpper(token))})
	return tx, sent, nil
}

func fmtAmount(x float64) string {
	if x >= 1 {
		return fmt.Sprintf("%.2f", x)
	}
	return fmt.Sprintf("%.6f", x)
}

// RevokeAll zeroes the Flash settlement allowance on USDC and WETH (live).
func (e *Engine) RevokeAll(ctx context.Context, walletID string) ([]string, error) {
	w, ok := e.Wallets.Get(walletID)
	if !ok {
		return nil, errors.New("unknown wallet")
	}
	if !e.Cfg.Live() {
		return nil, fmt.Errorf("%s mode never grants real allowances, nothing to revoke", e.Cfg.ModeName())
	}
	var txs []string
	for _, tok := range []string{e.Cfg.USDC, e.Cfg.WETH} {
		cur, err := e.Exec.Allowance(ctx, tok, w.Address())
		if err == nil && cur.Sign() == 0 {
			continue
		}
		tx, err := e.Exec.Revoke(ctx, w, tok)
		if err != nil {
			return txs, err
		}
		txs = append(txs, tx)
	}
	e.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%s revoked Flash's token allowances", walletID)})
	return txs, nil
}

// WalletView is everything the wallets page shows for one sub-wallet.
type WalletView struct {
	ID            string              `json:"id"`
	Role          string              `json:"role"`
	Address       string              `json:"address"`
	Mode          string              `json:"mode"` // paper | dry | live
	USDC          float64             `json:"usdc"`
	WETH          float64             `json:"weth"`
	ETH           *float64            `json:"eth,omitempty"`           // gas for approvals (live/dry)
	USDCAllowance *float64            `json:"usdcAllowance,omitempty"` // what Flash may pull right now
	WETHAllowance *float64            `json:"wethAllowance,omitempty"`
	EquityUSD     float64             `json:"equityUsd"`
	ReturnPct     float64             `json:"returnPct"`
	DrawdownPct   float64             `json:"drawdownPct"`
	SpentToday    float64             `json:"spentToday"`
	Profile       store.WalletProfile `json:"profile"`
	Follows       []store.Follow      `json:"follows"`
	OpenTrade     *store.Trade        `json:"openTrade,omitempty"`
	Trades        int                 `json:"trades"`
	PnLUSD        float64             `json:"pnlUsd"`
	Error         string              `json:"error,omitempty"`
	AgentName     string              `json:"agentName,omitempty"`
	Limits        map[string]any      `json:"limits"`
	Recent        []store.Trade       `json:"recent,omitempty"`
	Flags         map[string]bool     `json:"flags"`
}

// Wallet builds the view of one sub-wallet. Onchain reads (ETH, allowances)
// are only done when withChain is set (one wallet at a time, rate limits).
func (e *Engine) Wallet(ctx context.Context, w *wallet.Wallet, withChain bool) WalletView {
	v := WalletView{ID: w.ID, Role: string(w.Role), Address: w.Address(), Mode: e.Cfg.ModeName(), Flags: map[string]bool{}}
	if p, ok := e.AgentMap()[w.ID]; ok {
		v.AgentName = p.Name
	}
	u, we, err := e.Balances(ctx, w, 15*time.Second)
	if err != nil {
		v.Error = err.Error()
	}
	v.USDC, v.WETH = u, we
	v.EquityUSD = u + we*e.Prices.Last()
	pts := e.Store.PerfEquity(w.ID) // deposits / withdrawals excluded
	_, v.ReturnPct, _ = leaderboard.Stats(pts)
	v.DrawdownPct = leaderboard.CurrentDrawdown(pts)
	v.SpentToday = e.Store.CopySpentToday(w.ID)
	v.Profile, _ = e.Store.Profile(w.ID)
	v.Follows = e.Store.FollowsOf(w.ID)
	for _, t := range e.Store.Trades("", 0) {
		if t.WalletID != w.ID {
			continue
		}
		v.Trades++
		if t.PnLUSD != nil {
			v.PnLUSD += *t.PnLUSD
		}
		if t.Open() && v.OpenTrade == nil {
			tc := t
			v.OpenTrade = &tc
		}
		if len(v.Recent) < 5 {
			v.Recent = append(v.Recent, t)
		}
	}
	v.PnLUSD = math.Round(v.PnLUSD*100) / 100
	v.Limits = map[string]any{"maxTradeUsd": e.Cfg.MaxTradeUSD, "maxPositionPct": e.Cfg.MaxPositionPct,
		"maxTradesPerDay": e.Cfg.MaxTradesPerDay, "maxApprovalUsd": e.Cfg.MaxApprovalUSD}
	v.Flags["payoutLocked"] = v.Profile.Payout != ""
	v.Flags["paused"] = v.Profile.Paused
	if withChain && !e.Cfg.Paper {
		if eth, err := e.Exec.ETHBalance(ctx, w.Address()); err == nil {
			v.ETH = &eth
			v.Flags["needsGas"] = eth < 0.00005
		}
		if a, err := e.Exec.Allowance(ctx, e.Cfg.USDC, w.Address()); err == nil {
			f, _ := new(big.Float).Quo(new(big.Float).SetInt(a), big.NewFloat(1e6)).Float64()
			v.USDCAllowance = &f
		}
		if a, err := e.Exec.Allowance(ctx, e.Cfg.WETH, w.Address()); err == nil {
			f, _ := new(big.Float).Quo(new(big.Float).SetInt(a), big.NewFloat(1e18)).Float64()
			v.WETHAllowance = &f
		}
	}
	return v
}

// LiveQuote is the latest Flash streaming quote (WebSocket "quotes" channel)
// in the shape the UI shows: what MAX_TRADE_USD buys right now, all-in.
func (e *Engine) LiveQuote() map[string]any {
	if e.Stream == nil || e.Stream.Quote == nil {
		return nil
	}
	q, at := e.Stream.LiveQuote()
	if q == nil || time.Since(at) > 45*time.Second {
		return nil
	}
	mark, allIn, cost, pct, ok := quoteCosts(q, true)
	if !ok {
		return nil
	}
	return map[string]any{"usd": q.From.Amount.Float(), "weth": q.To.Amount.Float(), "mark": round2(mark), "allIn": round2(allIn),
		"costUsd": math.Round(cost*1000) / 1000, "costPct": math.Round(pct*100) / 100, "at": at.UTC(), "source": "Flash WebSocket quotes"}
}

// LiveBudgetCheck is budgetGate for callers outside Propose (CLI smoke).
func (e *Engine) LiveBudgetCheck(usd float64) string {
	if d := e.budgetGate(usd); !d.OK {
		return d.Reason
	}
	return ""
}

// who names the actor of an intent for the feed.
func (e *Engine) who(in referee.Intent) string {
	if in.PlanID != "" {
		if p, ok := e.Store.Plan(in.PlanID); ok {
			return fmt.Sprintf("%s's plan \"%s\"", p.Owner, p.Name)
		}
	}
	if in.WalletID != "" && in.WalletID != in.AgentID {
		return fmt.Sprintf("%s (wallet %s)", e.name(in.AgentID), in.WalletID)
	}
	return e.name(in.AgentID)
}

// InvalidateBalance drops a wallet's cached balances (after a paper top-up,
// so the next read and the next trade see the new money at once).
func (e *Engine) InvalidateBalance(walletID string) { e.invalidate(walletID) }

// detectFlow books money sent to (or taken from) a real wallet outside the
// arena, e.g. the owner funding it. Only when nothing traded since the last
// sample can a balance change not be trading, so only then is it a flow.
// Paper money only moves through the API, which books its own flows.
func (e *Engine) detectFlow(walletID string, eq float64) {
	if e.Cfg.Paper {
		return
	}
	prev, ok := e.Store.LastEquity(walletID)
	if !ok {
		return
	}
	for _, t := range e.Store.Trades("", 0) {
		if t.WalletID == walletID && (t.Open() || t.UpdatedAt.After(prev.At)) {
			return // trading may explain the change
		}
	}
	if d := eq - prev.USD - e.Store.FlowsSince(walletID, prev.At); math.Abs(d) > 0.05 {
		e.Store.AddFlow(walletID, d)
		e.Log.Info("external deposit/withdrawal booked", "wallet", walletID, "usd", round2(d))
	}
}

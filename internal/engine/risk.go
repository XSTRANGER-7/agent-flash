package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// The risk layer for automated entries: agent signals, copies, investor
// wallets and plans. Manual orders, test trades and people's own wallets are
// decisions a person makes on the spot and skip it (every other rule still
// applies to them).
//
//   - volatility guard: no new entries while the price swings too much
//   - losing-streak breaker: a wallet that lost N times in a row waits
//   - daily loss kill switch (live): today's realized losses past a limit
//     turn on the emergency stop
//   - maximum hold: an automated position is closed at market after a
//     time limit when neither its target nor its stop was reached

func automated(kind string) bool {
	return kind == "agent" || kind == "copy" || kind == "invest" || kind == "plan"
}

// riskGate runs the entry checks of the risk layer.
func (e *Engine) riskGate(in referee.Intent, w *wallet.Wallet, tokAddr string) referee.Decision {
	if !automated(tradeKind(in, w)) {
		return referee.Decision{OK: true}
	}
	if d := e.dailyLossGate(); !d.OK {
		return d
	}
	if d := e.volatilityGate(in.Token, tokAddr); !d.OK {
		return d
	}
	return e.lossStreakGate(w.ID)
}

// PriceMove is the % range (high vs low) of a coin over the last n checks.
func PriceMove(p []float64, n int) float64 {
	if n > 1 && len(p) > n {
		p = p[len(p)-n:]
	}
	if len(p) < 2 {
		return 0
	}
	lo, hi := p[0], p[0]
	for _, x := range p {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	if lo <= 0 {
		return 0
	}
	return (hi - lo) / lo * 100
}

func (e *Engine) volatilityGate(sym, addr string) referee.Decision {
	if e.Cfg.MaxMovePct <= 0 {
		return referee.Decision{OK: true}
	}
	p, fresh := e.tokenHistory(addr, 3*max(e.Cfg.PriceEvery, time.Second))
	if !fresh {
		return referee.Decision{OK: true} // stale prices are refused by the price guard
	}
	n := max(2, e.Cfg.MoveChecks)
	if move := PriceMove(p, n); move > e.Cfg.MaxMovePct {
		mins := time.Duration(n) * e.Cfg.PriceEvery
		return deny("VOLATILE", fmt.Sprintf("%s moved %.1f%% in the last %s (limit %.1f%%); waiting for calmer prices", sym, move, mins.Round(time.Minute), e.Cfg.MaxMovePct))
	}
	return referee.Decision{OK: true}
}

// LossStreak counts a wallet's most recent losing trades in a row and returns
// when the last of them closed. Test trades and dry runs do not count.
func LossStreak(trades []store.Trade, walletID string) (int, time.Time) {
	n, last := 0, time.Time{}
	for _, t := range trades { // newest first
		if t.WalletID != walletID || t.Status != "closed" || t.PnLUSD == nil || t.DryRun || t.Test {
			continue
		}
		if *t.PnLUSD >= 0 {
			break
		}
		n++
		if last.IsZero() || t.UpdatedAt.After(last) {
			last = t.UpdatedAt
		}
	}
	return n, last
}

func (e *Engine) lossStreakGate(walletID string) referee.Decision {
	if e.Cfg.MaxLossStreak <= 0 || e.Cfg.LossStreakPause <= 0 {
		return referee.Decision{OK: true}
	}
	n, last := LossStreak(e.Store.Trades("", 0), walletID)
	if n >= e.Cfg.MaxLossStreak && time.Since(last) < e.Cfg.LossStreakPause {
		return deny("LOSS_STREAK", fmt.Sprintf("%d losing trades in a row; automated trading for this wallet resumes at %s",
			n, last.Add(e.Cfg.LossStreakPause).Local().Format("15:04")))
	}
	return referee.Decision{OK: true}
}

// LiveLossToday is the realized profit and loss of real-money trades closed
// today (the server's local day). People's own wallets are their business.
func (e *Engine) LiveLossToday() float64 {
	y, m, d := time.Now().Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	sum := 0.0
	for _, t := range e.Store.Trades("", 0) {
		if t.Paper || t.DryRun || t.Kind == "self" || t.Status != "closed" || t.PnLUSD == nil || t.UpdatedAt.Before(start) {
			continue
		}
		sum += *t.PnLUSD
	}
	return math.Round(sum*100) / 100
}

func (e *Engine) dailyLossGate() referee.Decision {
	if !e.Cfg.Live() || e.Cfg.DailyLossLimitUSD <= 0 {
		return referee.Decision{OK: true}
	}
	if pnl := e.LiveLossToday(); pnl <= -e.Cfg.DailyLossLimitUSD {
		return deny("DAILY_LOSS", fmt.Sprintf("$%.2f lost today, at the daily loss limit of $%.2f; trading stays stopped until an operator resumes it", -pnl, e.Cfg.DailyLossLimitUSD))
	}
	return referee.Decision{OK: true}
}

// holdLimit is how long an automated trade may stay open (0 = no limit): the
// agent maker's own setting, else MAX_HOLD_HOURS.
func (e *Engine) holdLimit(t store.Trade) time.Duration {
	if !automated(t.Kind) {
		return 0
	}
	if t.MaxHoldMin > 0 {
		return time.Duration(t.MaxHoldMin) * time.Minute
	}
	return time.Duration(e.Cfg.MaxHoldHours * float64(time.Hour))
}

// Overdue lists open positions held past their time limit.
func (e *Engine) Overdue(now time.Time) []store.Trade {
	var out []store.Trade
	for _, t := range e.Store.Trades("", 0) {
		if t.DryRun || t.ExitOrderID != "" || (t.Status != "filled" && t.Status != "unprotected") {
			continue
		}
		if lim := e.holdLimit(t); lim > 0 && now.Sub(t.CreatedAt) > lim {
			out = append(out, t)
		}
	}
	return out
}

// RunRisk enforces the daily loss kill switch and the maximum hold time.
func (e *Engine) RunRisk(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	tries := map[string]int{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if d := e.dailyLossGate(); !d.OK {
			if on, _ := e.Halted(); !on {
				e.Halt(ctx, "daily loss limit reached")
			}
		}
		for _, tr := range e.Overdue(time.Now()) {
			if tries[tr.ID] >= 3 {
				continue // tell the operator once, then leave it to them
			}
			tries[tr.ID]++
			lim := e.holdLimit(tr)
			why := fmt.Sprintf("time limit: held %s (limit %s)", time.Since(tr.CreatedAt).Round(time.Minute), lim.Round(time.Minute))
			e.Store.UpdateTrade(tr.ID, func(x *store.Trade) { x.ExitWhy = why })
			if _, err := e.ExitPosition(ctx, tr.ID); err != nil {
				e.Log.Warn("time-limit exit failed", "trade", tr.ID, "err", err)
				if tries[tr.ID] == 3 {
					e.Feed.Publish(feed.Post{Kind: "trade_blocked", AgentID: tr.AgentID, TradeID: tr.ID,
						Text: fmt.Sprintf("%s passed its time limit but could not be closed automatically (%s). The operator can use Exit now.", e.label(tr), shortErr(err))})
				}
				continue
			}
			e.Log.Info("time-limit exit", "trade", tr.ID, "held", time.Since(tr.CreatedAt).Round(time.Minute))
		}
	}
}

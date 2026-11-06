package engine

// Personal plans: a person's own automated trading. They pick the coin, the
// money per trade, the budget they are willing to lose, take profit, stop
// loss, slippage, how many trades a day and how long it runs; the engine
// trades their sub-wallet on the chosen strategy's signals. "auto" lets the
// arena pick, each time, the strategy that is winning on the leaderboard.
// Every order still goes through the referee, the guards and the signer.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
)

// PlanStrategies are the brains a plan can use.
var PlanStrategies = map[string]string{
	"auto":       "AI auto-pilot: uses whichever arena strategy has the best Arena Score right now",
	"trend":      "Trend: buys when the short average crosses above the long one",
	"dip":        "Dip: buys after a 1.5% drop from the recent high",
	"contrarian": "Contrarian: buys after 4 falling ticks in a row",
	"interval":   "Interval: buys every N minutes when no trade is open (steady, like DCA)",
}

// PlanStats are derived from the plan's trades.
type PlanStats struct {
	Trades   int     `json:"trades"`
	Open     int     `json:"open"`
	Closed   int     `json:"closed"`
	Wins     int     `json:"wins"`
	PnLUSD   float64 `json:"pnlUsd"`
	Today    int     `json:"today"`
	SpentUSD float64 `json:"spentUsd"` // in open positions right now
}

func (e *Engine) PlanStats(id string) (PlanStats, []store.Trade) {
	var s PlanStats
	var trades []store.Trade
	day := time.Now().UTC().Format("2006-01-02")
	for _, t := range e.Store.Trades("", 0) {
		if t.PlanID != id {
			continue
		}
		trades = append(trades, t)
		s.Trades++
		if t.CreatedAt.UTC().Format("2006-01-02") == day {
			s.Today++
		}
		if t.Open() {
			s.Open++
			s.SpentUSD += t.SizeUSD
		}
		if t.PnLUSD != nil {
			s.Closed++
			s.PnLUSD += *t.PnLUSD
			if *t.PnLUSD > 0 {
				s.Wins++
			}
		}
	}
	s.PnLUSD = math.Round(s.PnLUSD*100) / 100
	return s, trades
}

// bestStrategy is the auto-pilot's pick: the agent strategy with the best
// Arena Score now (ties: most closed trades, then trend).
func (e *Engine) bestStrategy() (string, string) {
	best, bestScore, bestClosed := "trend", math.Inf(-1), -1
	for id, p := range e.Agents {
		_, ret, mdd := leaderboard.Stats(e.Store.PerfEquity(id))
		score := leaderboard.Score(ret, mdd)
		closed := 0
		for _, t := range e.Store.Trades(id, 0) {
			if t.WalletID == id && t.PnLUSD != nil {
				closed++
			}
		}
		if score > bestScore || (score == bestScore && closed > bestClosed) || (score == bestScore && closed == bestClosed && p.Strategy < best) {
			best, bestScore, bestClosed = p.Strategy, score, closed
		}
	}
	return best, fmt.Sprintf("auto-pilot picked %s (best Arena Score %.2f)", best, bestScore)
}

func strategyFor(name string) arena.Strategy {
	switch name {
	case "dip":
		return arena.DipBuyer{Window: 20, DipPct: 1.5}
	case "contrarian":
		return arena.Contrarian{Streak: 4}
	}
	return arena.TrendHunter{Fast: 5, Slow: 20}
}

// RunPlans checks every active plan on the agent cadence.
func (e *Engine) RunPlans(ctx context.Context) {
	t := time.NewTicker(e.Cfg.AgentEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, p := range e.Store.Plans() {
			if p.Status == "active" {
				e.StepPlan(ctx, p.ID)
			}
		}
	}
}

func (e *Engine) setPlan(id, status, reason string) {
	p, ok := e.Store.UpdatePlan(id, func(x *store.Plan) { x.Status, x.StatusReason = status, reason })
	if ok {
		e.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%s's plan \"%s\" %s: %s", p.Owner, p.Name, status, reason)})
	}
}

func (e *Engine) noteSignal(id, s string) {
	e.Store.UpdatePlan(id, func(x *store.Plan) { x.LastSignal, x.LastSignalAt = s, time.Now().UTC() })
}

// StepPlan runs one decision for a plan. It is exported for the API ("check
// now") and tests; RunPlans calls it on a timer.
func (e *Engine) StepPlan(ctx context.Context, id string) {
	p, ok := e.Store.Plan(id)
	if !ok || p.Status != "active" {
		return
	}
	now := time.Now()
	stats, trades := e.PlanStats(id)
	if !p.EndsAt.IsZero() && now.After(p.EndsAt) {
		e.setPlan(id, "ended", "its time window is over")
		if p.ExitAtEnd {
			for _, t := range trades {
				if t.Status == "filled" || t.Status == "unprotected" {
					if _, err := e.ExitPosition(ctx, t.ID); err != nil {
						e.Log.Warn("plan end exit failed", "plan", id, "trade", t.ID, "err", err)
					}
				}
			}
		}
		return
	}
	if now.Before(p.StartsAt) {
		e.noteSignal(id, "waiting for the start time")
		return
	}
	if p.LossLimitPct > 0 && stats.PnLUSD <= -p.BudgetUSD*p.LossLimitPct/100 {
		e.setPlan(id, "stopped", fmt.Sprintf("lost $%.2f, past its %.0f%% loss limit", -stats.PnLUSD, p.LossLimitPct))
		return
	}
	if left := p.BudgetUSD + stats.PnLUSD; left < p.TradeUSD {
		e.setPlan(id, "stopped", fmt.Sprintf("budget used up ($%.2f left, trades are $%.2f)", math.Max(left, 0), p.TradeUSD))
		return
	}
	if stats.Open > 0 {
		e.noteSignal(id, "holding a position; waiting for take profit or stop loss")
		return
	}
	if p.MaxTradesPerDay > 0 && stats.Today >= p.MaxTradesPerDay {
		e.noteSignal(id, fmt.Sprintf("done for today (%d trades)", stats.Today))
		return
	}
	if e.Cfg.Live() && !e.Cfg.LiveAgents {
		e.noteSignal(id, "live auto-trading is off (LIVE_AGENTS=false); no real orders")
		return
	}
	tok, ok := e.tokenBySymbol(p.Token)
	if !ok {
		e.setPlan(id, "stopped", p.Token+" is no longer tradeable")
		return
	}
	prices, fresh := e.tokenHistory(tok.Address, 3*e.Cfg.PriceEvery)
	if !fresh || len(prices) == 0 {
		e.noteSignal(id, "waiting for a fresh "+p.Token+" price")
		return
	}
	px := prices[len(prices)-1]
	strat, why := p.Strategy, ""
	var reason string
	switch strat {
	case "interval":
		wait := time.Duration(max(p.IntervalMin, 1)) * time.Minute
		if !p.LastTradeAt.IsZero() && now.Sub(p.LastTradeAt) < wait {
			e.noteSignal(id, fmt.Sprintf("next entry in %s", (wait-now.Sub(p.LastTradeAt)).Round(time.Second)))
			return
		}
		reason = fmt.Sprintf("interval entry (every %d min)", max(p.IntervalMin, 1))
	default:
		if strat == "auto" {
			strat, why = e.bestStrategy()
		}
		sig := strategyFor(strat).Decide(prices)
		if sig == nil {
			msg := "no " + strat + " signal yet on " + p.Token
			if why != "" {
				msg = why + "; " + msg
			}
			e.noteSignal(id, msg)
			return
		}
		reason = sig.Reason
		if why != "" {
			reason = why + ": " + reason
		}
	}
	slip := ""
	if p.MaxSlippagePct > 0 {
		slip = strconv.FormatFloat(p.MaxSlippagePct/100, 'f', -1, 64)
	}
	in := referee.Intent{
		WalletID: p.WalletID, PlanID: id, Token: tok.Symbol,
		SizeUSD: p.TradeUSD, OwnSize: true, MaxSlippage: slip, Price: px, Reason: reason,
		Style: p.Style, TwapMinutes: p.TwapMinutes, ProtectMode: p.ProtectMode, TrailPct: p.TrailPct,
	}
	entry := px // targets are set from where the entry happens
	switch p.Style {
	case "limit":
		in.EntryLimit = roundPx(px * (1 - p.LimitPct/100))
		entry = in.EntryLimit
	case "stop":
		in.StopPrice = roundPx(px * (1 + p.StopPct/100))
		entry = in.StopPrice
	}
	in.TakeProfit, in.StopLoss = roundPx(entry*(1+p.TakeProfitPct/100)), roundPx(entry*(1-p.StopLossPct/100))
	res := e.Propose(ctx, in)
	switch {
	case res.Trade != nil:
		e.Store.UpdatePlan(id, func(x *store.Plan) {
			x.LastTradeAt, x.LastSignal, x.LastSignalAt = now.UTC(), "entered: "+reason, now.UTC()
		})
	case res.Error != "":
		e.noteSignal(id, "order not placed: "+shortErr(fmt.Errorf("%s", res.Error)))
	default:
		e.noteSignal(id, "blocked by the referee: "+res.Decision.Reason)
	}
}

// roundPx keeps 2 decimals for normal prices and more for tiny ones.
func roundPx(x float64) float64 {
	if x >= 1 {
		return math.Round(x*100) / 100
	}
	return math.Round(x*1e8) / 1e8
}

// InvestAll trades every investor wallet of in.AgentID on this signal.
func (e *Engine) InvestAll(ctx context.Context, in referee.Intent) { e.investAll(ctx, in) }

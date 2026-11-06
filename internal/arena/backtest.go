package arena

import "math"

// Backtest replays a strategy over past prices the way the arena trades it:
// one position at a time, a decision every `every` price checks, take profit
// and stop loss checked on every price, and Flash's cost paid on the buy and
// on the sell. It is a rough guide, not a promise: real fills, costs and
// timing differ, and a few hours of history is a small sample.
type BacktestOptions struct {
	SizeUSD      float64 // dollars per trade
	CostPerOrder float64 // Flash's fixed cost per order in USD (buy and sell each pay it)
	FeePct       float64 // integrator fee, % of size per order
	Every        int     // decide every N price checks (agent cadence / price cadence)
	NetTargets   bool    // raise TP by 2x the round-trip cost %, like NET_TARGETS
	Warmup       int     // first price index the strategy may act on
	Cooldown     int     // after a losing exit, skip this many price checks
}

type BacktestTrade struct {
	In, Out    int     // price indexes (Out = -1 while open at the end)
	Entry      float64 `json:"entry"`
	Exit       float64 `json:"exit"`
	TP         float64 `json:"tp"`
	SL         float64 `json:"sl"`
	PnLUSD     float64 `json:"pnlUsd"`
	CostUSD    float64 `json:"costUsd"`
	Why        string  `json:"why"` // tp | sl | open
	Reason     string  `json:"reason"`
	InIndex    int     `json:"in"`
	OutIndex   int     `json:"out"`
	HeldChecks int     `json:"heldChecks"`
}

type BacktestResult struct {
	Points     int             `json:"points"`
	Trades     []BacktestTrade `json:"trades"`
	Wins       int             `json:"wins"`
	Losses     int             `json:"losses"`
	Open       int             `json:"open"`
	NetUSD     float64         `json:"netUsd"`
	GrossUSD   float64         `json:"grossUsd"`
	CostUSD    float64         `json:"costUsd"`
	ReturnPct  float64         `json:"returnPct"` // net, on the trade size
	HoldPct    float64         `json:"holdPct"`   // buy at the start, hold to the end (no costs)
	WinRatePct float64         `json:"winRatePct"`
	MaxLossRun int             `json:"maxLossRun"` // most losses in a row
}

func Backtest(s Strategy, prices []float64, o BacktestOptions) BacktestResult {
	r := BacktestResult{Points: len(prices), Trades: []BacktestTrade{}}
	if len(prices) < 2 || o.SizeUSD <= 0 {
		return r
	}
	if o.Every < 1 {
		o.Every = 1
	}
	orderCost := o.CostPerOrder + o.SizeUSD*o.FeePct/100
	var open *BacktestTrade
	run, waitUntil := 0, 0
	closeAt := func(i int, px float64, why string) {
		t := open
		t.Out, t.OutIndex, t.Exit, t.Why, t.HeldChecks = i, i, px, why, i-t.In
		gross := o.SizeUSD * (px/t.Entry - 1)
		t.CostUSD = 2 * orderCost
		t.PnLUSD = gross - t.CostUSD
		r.GrossUSD += gross
		r.CostUSD += t.CostUSD
		r.NetUSD += t.PnLUSD
		if t.PnLUSD > 0 {
			r.Wins++
			run = 0
		} else {
			r.Losses++
			run++
			r.MaxLossRun = max(r.MaxLossRun, run)
			waitUntil = i + o.Cooldown
		}
		r.Trades = append(r.Trades, *t)
		open = nil
	}
	for i := max(1, o.Warmup); i < len(prices); i++ {
		px := prices[i]
		if open != nil {
			switch {
			case px <= open.SL:
				closeAt(i, px, "sl") // a stop sells at the market, so a gap past it costs more
			case px >= open.TP:
				closeAt(i, open.TP, "tp")
			}
			continue
		}
		if i%o.Every != 0 || i < waitUntil {
			continue
		}
		sig := s.Decide(prices[:i+1])
		if sig == nil {
			continue
		}
		tp := sig.TPPct
		if o.NetTargets && o.SizeUSD > 0 {
			tp += 2 * orderCost / o.SizeUSD * 100 // 2x the entry cost %, as NET_TARGETS adds it
		}
		open = &BacktestTrade{In: i, InIndex: i, Out: -1, OutIndex: -1, Entry: px, TP: px * (1 + tp/100), SL: px * (1 - sig.SLPct/100), Reason: sig.Reason}
	}
	if open != nil {
		last := prices[len(prices)-1]
		gross := o.SizeUSD * (last/open.Entry - 1)
		open.Exit, open.Why, open.HeldChecks = last, "open", len(prices)-1-open.In
		open.CostUSD = 2 * orderCost
		open.PnLUSD = gross - open.CostUSD // as if sold now
		r.Open = 1
		r.Trades = append(r.Trades, *open)
	}
	if done := r.Wins + r.Losses; done > 0 {
		r.WinRatePct = float64(r.Wins) / float64(done) * 100
	}
	r.ReturnPct = r.NetUSD / o.SizeUSD * 100
	r.HoldPct = (prices[len(prices)-1]/prices[0] - 1) * 100
	r.NetUSD, r.GrossUSD, r.CostUSD = round4(r.NetUSD), round4(r.GrossUSD), round4(r.CostUSD)
	return r
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

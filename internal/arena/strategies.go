// Package arena holds the competing agents: their profiles, a shared price
// feed and the strategies. Strategies are deterministic code (they never
// refuse to trade, unlike LLMs); an optional Reasoner writes the post text.
package arena

import (
	"fmt"
	"math"
)

type Signal struct {
	TPPct   float64 // take profit, % above price
	SLPct   float64 // stop loss, % below price
	SizePct float64
	Reason  string
}

type Strategy interface {
	Name() string
	Decide(prices []float64) *Signal // nil = no trade
}

func sma(p []float64, n int) float64 {
	if len(p) < n || n == 0 {
		return 0
	}
	s := 0.0
	for _, x := range p[len(p)-n:] {
		s += x
	}
	return s / float64(n)
}

// TrendHunter: fast SMA crosses above slow SMA.
type TrendHunter struct{ Fast, Slow int }

func (TrendHunter) Name() string { return "trend" }
func (t TrendHunter) Decide(p []float64) *Signal {
	if len(p) < t.Slow+1 {
		return nil
	}
	prevF, prevS := sma(p[:len(p)-1], t.Fast), sma(p[:len(p)-1], t.Slow)
	f, s := sma(p, t.Fast), sma(p, t.Slow)
	if prevF <= prevS && f > s {
		return &Signal{TPPct: 3, SLPct: 2, SizePct: 20, Reason: fmt.Sprintf("Fast average (%d) crossed above slow (%d). Riding the trend with a 2%% stop.", t.Fast, t.Slow)}
	}
	return nil
}

// DipBuyer: price falls X% below the recent high.
type DipBuyer struct {
	Window int
	DipPct float64
}

func (DipBuyer) Name() string { return "dip" }
func (d DipBuyer) Decide(p []float64) *Signal {
	if len(p) < d.Window {
		return nil
	}
	hi := 0.0
	for _, x := range p[len(p)-d.Window:] {
		hi = math.Max(hi, x)
	}
	last := p[len(p)-1]
	if drop := (hi - last) / hi * 100; drop >= d.DipPct {
		return &Signal{TPPct: 2, SLPct: 2.5, SizePct: 20, Reason: fmt.Sprintf("Down %.1f%% from the recent high. Buying the dip, target +2%%.", drop)}
	}
	return nil
}

// Contrarian: N consecutive down ticks, bet on a bounce.
type Contrarian struct{ Streak int }

func (Contrarian) Name() string { return "contrarian" }
func (c Contrarian) Decide(p []float64) *Signal {
	if len(p) < c.Streak+1 {
		return nil
	}
	for i := len(p) - c.Streak; i < len(p); i++ {
		if p[i] >= p[i-1] {
			return nil
		}
	}
	return &Signal{TPPct: 1.5, SLPct: 1.5, SizePct: 15, Reason: fmt.Sprintf("%d red ticks in a row. Everyone is selling, so I'm buying a small bounce.", c.Streak)}
}

// Pullback buys a dip only while the longer trend is rising: price is DipPct
// below its recent high AND the long average is higher than it was a quarter
// of its length ago. A fall through the long average is a breakdown, not a
// dip, and is skipped. Fewer, better-placed trades than buying every drop.
type Pullback struct {
	Window int     // recent high over this many checks
	DipPct float64 // how far below that high
	Trend  int     // long average length (checks)
}

func (Pullback) Name() string { return "pullback" }
func (b Pullback) Decide(p []float64) *Signal {
	lag := max(1, b.Trend/4)
	if len(p) < max(b.Window, b.Trend+lag) {
		return nil
	}
	hi := 0.0
	for _, x := range p[len(p)-b.Window:] {
		hi = math.Max(hi, x)
	}
	last := p[len(p)-1]
	drop := (hi - last) / hi * 100
	if drop < b.DipPct {
		return nil
	}
	long, before := sma(p, b.Trend), sma(p[:len(p)-lag], b.Trend)
	if long <= before || last < long*(1-b.DipPct/100) {
		return nil // trend not rising, or price broke down through it
	}
	return &Signal{TPPct: 2, SLPct: 1.2, SizePct: 20, Reason: fmt.Sprintf("Uptrend (%d-check average rising) and a %.2f%% pullback from the recent high. Buying the dip inside the trend.", b.Trend, drop)}
}

// Profile is the public face of an agent.
type Profile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tagline  string   `json:"tagline"`
	Strategy string   `json:"strategy"`
	Address  string   `json:"address"`
	Strat    Strategy `json:"-"`
	// Community agents are made by people (not built in); Owner is the
	// creator's own address.
	Community bool   `json:"community,omitempty"`
	Owner     string `json:"owner,omitempty"`
	// Token is the coin it trades (symbol); empty = the arena's trade asset.
	Token string `json:"token,omitempty"`
}

// Tuned runs a strategy with a person's own take profit, stop loss and size
// (0 keeps the strategy's value).
type Tuned struct {
	Inner                 Strategy
	TPPct, SLPct, SizePct float64
}

func (t Tuned) Name() string { return t.Inner.Name() }

func (t Tuned) Decide(p []float64) *Signal {
	s := t.Inner.Decide(p)
	if s == nil {
		return nil
	}
	c := *s
	if t.TPPct > 0 {
		c.TPPct = t.TPPct
	}
	if t.SLPct > 0 {
		c.SLPct = t.SLPct
	}
	if t.SizePct > 0 {
		c.SizePct = t.SizePct
	}
	return &c
}

// StrategyParams is what a person can tune when they make an agent. Zero
// values take the built-in agent's defaults.
type StrategyParams struct {
	Kind                  string // trend | dip | contrarian | pullback
	Fast, Slow            int    // trend: moving-average lengths (price checks)
	Window                int    // dip, pullback: how far back the recent high is looked for
	Trend                 int    // pullback: long average that must be rising (checks)
	DipPct                float64
	Streak                int // contrarian: falling checks in a row
	TPPct, SLPct, SizePct float64
}

// BuildStrategy checks a person's settings against the arena's limits and
// returns the strategy plus the settings with defaults filled in.
func BuildStrategy(p StrategyParams) (Strategy, StrategyParams, error) {
	in := func(v, lo, hi float64) bool { return v >= lo && v <= hi }
	var inner Strategy
	switch p.Kind {
	case "trend":
		if p.Fast == 0 {
			p.Fast = 5
		}
		if p.Slow == 0 {
			p.Slow = 20
		}
		if !in(float64(p.Fast), 2, 30) || !in(float64(p.Slow), 3, 120) || p.Slow <= p.Fast {
			return nil, p, fmt.Errorf("trend: fast 2-30 checks, slow 3-120 and longer than fast")
		}
		inner = TrendHunter{Fast: p.Fast, Slow: p.Slow}
	case "dip":
		if p.Window == 0 {
			p.Window = 20
		}
		if p.DipPct == 0 {
			p.DipPct = 1.5
		}
		if !in(float64(p.Window), 5, 240) || !in(p.DipPct, 0.2, 20) {
			return nil, p, fmt.Errorf("dip: look back 5-240 checks, drop 0.2%%-20%%")
		}
		inner = DipBuyer{Window: p.Window, DipPct: p.DipPct}
	case "contrarian":
		if p.Streak == 0 {
			p.Streak = 4
		}
		if !in(float64(p.Streak), 2, 20) {
			return nil, p, fmt.Errorf("contrarian: 2-20 falling checks in a row")
		}
		inner = Contrarian{Streak: p.Streak}
	case "pullback":
		if p.Window == 0 {
			p.Window = 40
		}
		if p.DipPct == 0 {
			p.DipPct = 0.5
		}
		if p.Trend == 0 {
			p.Trend = 120
		}
		// The price feed keeps 500 checks: trend + trend/4 must fit in it.
		if !in(float64(p.Window), 5, 240) || !in(p.DipPct, 0.1, 10) || !in(float64(p.Trend), 20, 360) {
			return nil, p, fmt.Errorf("pullback: look back 5-240 checks, drop 0.1%%-10%%, trend 20-360 checks")
		}
		inner = Pullback{Window: p.Window, DipPct: p.DipPct, Trend: p.Trend}
	default:
		return nil, p, fmt.Errorf("strategy must be trend, dip, contrarian or pullback")
	}
	switch {
	case !in(p.TPPct, 0.6, 20):
		return nil, p, fmt.Errorf("take profit must be 0.6%%-20%%")
	case !in(p.SLPct, 0.3, 5):
		return nil, p, fmt.Errorf("stop loss must be 0.3%%-5%% (every trade has a stop within 5%%)")
	case !in(p.SizePct, 5, 100):
		return nil, p, fmt.Errorf("size must be 5%%-100%% of the account per trade")
	}
	// Keep only the settings this strategy uses, so stray values from the
	// form are never stored or shown.
	c := StrategyParams{Kind: p.Kind, TPPct: p.TPPct, SLPct: p.SLPct, SizePct: p.SizePct}
	switch p.Kind {
	case "trend":
		c.Fast, c.Slow = p.Fast, p.Slow
	case "dip":
		c.Window, c.DipPct = p.Window, p.DipPct
	case "contrarian":
		c.Streak = p.Streak
	case "pullback":
		c.Window, c.DipPct, c.Trend = p.Window, p.DipPct, p.Trend
	}
	return Tuned{Inner: inner, TPPct: p.TPPct, SLPct: p.SLPct, SizePct: p.SizePct}, c, nil
}

// DefaultRoster maps wallet ids in wallets.json to strategies.
func DefaultRoster() map[string]Profile {
	return map[string]Profile{
		"trendhunter": {ID: "trendhunter", Name: "TrendHunter", Tagline: "Follows the trend, always with a stop.", Strategy: "trend", Strat: TrendHunter{Fast: 5, Slow: 20}},
		"dipbuyer":    {ID: "dipbuyer", Name: "DipBuyer", Tagline: "Buys fear, sells relief.", Strategy: "dip", Strat: DipBuyer{Window: 20, DipPct: 1.5}},
		"contrarian":  {ID: "contrarian", Name: "Contrarian Kaka", Tagline: "If everyone sells, I buy.", Strategy: "contrarian", Strat: Contrarian{Streak: 4}},
	}
}

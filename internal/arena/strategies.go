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

// Profile is the public face of an agent.
type Profile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tagline  string   `json:"tagline"`
	Strategy string   `json:"strategy"`
	Address  string   `json:"address"`
	Strat    Strategy `json:"-"`
}

// DefaultRoster maps wallet ids in wallets.json to strategies.
func DefaultRoster() map[string]Profile {
	return map[string]Profile{
		"trendhunter": {ID: "trendhunter", Name: "TrendHunter", Tagline: "Follows the trend, always with a stop.", Strategy: "trend", Strat: TrendHunter{Fast: 5, Slow: 20}},
		"dipbuyer":    {ID: "dipbuyer", Name: "DipBuyer", Tagline: "Buys fear, sells relief.", Strategy: "dip", Strat: DipBuyer{Window: 20, DipPct: 1.5}},
		"contrarian":  {ID: "contrarian", Name: "Contrarian Kaka", Tagline: "If everyone sells, I buy.", Strategy: "contrarian", Strat: Contrarian{Streak: 4}},
	}
}

package engine

import (
	"fmt"
	"strconv"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/store"
)

// CheckAgentLimits checks an agent maker's Advanced settings against hard
// caps. Zero / empty values mean "use the arena's setting". Makers can tune
// their own agent, but never past these caps, never above the arena's
// largest trade, and the stop loss rule (within 5%) is not part of this at all.
// ArenaCostPct is the arena's entry cost limit (%), 3 when unset.
func ArenaCostPct(cfg *config.Config) float64 {
	if cfg.MaxEntryCostPct > 0 {
		return cfg.MaxEntryCostPct
	}
	return 3
}

// ArenaSlippagePct is MAX_SLIPPAGE as a percent, 3 when unset.
func ArenaSlippagePct(cfg *config.Config) float64 {
	if v, err := strconv.ParseFloat(cfg.MaxSlippage, 64); err == nil && v > 0 {
		return v * 100
	}
	return 3
}

func CheckAgentLimits(cfg *config.Config, l store.AgentLimits) (store.AgentLimits, error) {
	in := func(v, lo, hi float64) bool { return v == 0 || (v >= lo && v <= hi) }
	tri := func(s string) bool { return s == "" || s == "on" || s == "off" }
	// Tighten only: followers trade under the agent's cost and slippage
	// limits, so they never pass the arena's own.
	maxCost, maxSlip, maxDay := ArenaCostPct(cfg), ArenaSlippagePct(cfg), cfg.MaxTradesPerDay
	if maxDay <= 0 {
		maxDay = 100
	}
	switch {
	case !in(l.MaxCostPct, 0.2, maxCost):
		return l, fmt.Errorf("cost limit: 0.2%%-%.1f%% (the arena's limit; empty = %.1f%%)", maxCost, maxCost)
	case !in(l.SlippagePct, 0.1, maxSlip):
		return l, fmt.Errorf("slippage: 0.1%%-%.1f%% (the arena's limit)", maxSlip)
	case l.TradesPerDay < 0 || l.TradesPerDay > maxDay:
		return l, fmt.Errorf("trades a day: 1-%d (the arena's limit)", maxDay)
	case !in(l.MinTradeUSD, 1, cfg.MaxTradeUSD):
		return l, fmt.Errorf("smallest trade: $1-$%.0f", cfg.MaxTradeUSD)
	case !in(l.MaxTradeUSD, 1, cfg.MaxTradeUSD):
		return l, fmt.Errorf("largest trade: $1-$%.0f (the arena's largest)", cfg.MaxTradeUSD)
	case l.MinTradeUSD > 0 && l.MaxTradeUSD > 0 && l.MinTradeUSD > l.MaxTradeUSD:
		return l, fmt.Errorf("smallest trade is above the largest")
	case l.CooldownMin < 0 || l.CooldownMin > 1440:
		return l, fmt.Errorf("cooldown after a loss: 0-1440 minutes")
	case l.MaxHoldMin < 0 || (l.MaxHoldMin > 0 && l.MaxHoldMin < 5) || l.MaxHoldMin > 10080:
		return l, fmt.Errorf("time limit per trade: 5-10080 minutes (7 days), or empty for none")
	case !tri(l.NetTargets) || !tri(l.BreakEven):
		return l, fmt.Errorf("net targets and break-even stop: on, off or empty")
	}
	return l, nil
}

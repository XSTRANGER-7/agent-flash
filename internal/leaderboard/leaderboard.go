// Package leaderboard computes the risk-adjusted Arena Score.
//
//	Arena Score = Return% - 0.5 * MaxDrawdown%
//
// so a smart +12% with a 4% drawdown (score 10) beats a reckless +20%
// with a 30% drawdown (score 5).
package leaderboard

import (
	"sort"

	"github.com/vikram/agent-arena/internal/store"
)

type Row struct {
	Rank        int     `json:"rank"`
	AgentID     string  `json:"agentId"`
	Name        string  `json:"name"`
	EquityUSD   float64 `json:"equityUsd"`
	ReturnPct   float64 `json:"returnPct"`
	MaxDrawdown float64 `json:"maxDrawdownPct"`
	Score       float64 `json:"score"`
	Trades      int     `json:"trades"`
	Closed      int     `json:"closed"`
	Wins        int     `json:"wins"`
	WinRatePct  float64 `json:"winRatePct"`
	Followers   int     `json:"followers"`
}

// Stats derives return and max drawdown from an equity series.
func Stats(pts []store.EquityPoint) (equity, ret, mdd float64) {
	if len(pts) == 0 {
		return 0, 0, 0
	}
	start := pts[0].USD
	peak := start
	for _, p := range pts {
		if p.USD > peak {
			peak = p.USD
		}
		if peak > 0 {
			if dd := (peak - p.USD) / peak * 100; dd > mdd {
				mdd = dd
			}
		}
	}
	equity = pts[len(pts)-1].USD
	if start > 0 {
		ret = (equity - start) / start * 100
	}
	return
}

func Score(ret, mdd float64) float64 { return ret - 0.5*mdd }

// CurrentDrawdown is how far the latest equity sits below its peak (%).
func CurrentDrawdown(pts []store.EquityPoint) float64 {
	peak := 0.0
	for _, p := range pts {
		if p.USD > peak {
			peak = p.USD
		}
	}
	if peak <= 0 || len(pts) == 0 {
		return 0
	}
	return (peak - pts[len(pts)-1].USD) / peak * 100
}

// Rank sorts by score desc, then return desc.
func Rank(rows []Row) []Row {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].ReturnPct != rows[j].ReturnPct {
			return rows[i].ReturnPct > rows[j].ReturnPct
		}
		if rows[i].Closed != rows[j].Closed {
			return rows[i].Closed > rows[j].Closed
		}
		// Deterministic last resort: rows come from a map, so ties would
		// otherwise reshuffle on every request (and fake rank changes).
		return rows[i].AgentID < rows[j].AgentID
	})
	for i := range rows {
		rows[i].Rank = i + 1
	}
	return rows
}

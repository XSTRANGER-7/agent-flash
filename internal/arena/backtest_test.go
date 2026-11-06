package arena

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// A rising market with a pullback: the pullback strategy buys the dip, the
// bounce hits take profit, and Flash's cost is paid on the buy and the sell.
func TestBacktestPaysCostsAndHitsTarget(t *testing.T) {
	var p []float64
	for i := 0; i < 200; i++ { // slow uptrend
		p = append(p, 2000+float64(i)*0.5)
	}
	top := p[len(p)-1]
	p = append(p, top*0.996, top*0.993) // 0.7% pullback
	for i := 1; i <= 40; i++ {          // bounce well past +2%
		p = append(p, top*0.993*(1+float64(i)*0.001))
	}
	s, _, err := BuildStrategy(StrategyParams{Kind: "pullback", Window: 20, DipPct: 0.5, Trend: 60, TPPct: 2, SLPct: 1, SizePct: 50})
	if err != nil {
		t.Fatal(err)
	}
	r := Backtest(s, p, BacktestOptions{SizeUSD: 10, CostPerOrder: 0.03, Every: 1})
	if len(r.Trades) != 1 || r.Wins != 1 || r.Trades[0].Why != "tp" {
		t.Fatalf("want one take-profit win, got %+v", r)
	}
	tr := r.Trades[0]
	want := 10*(tr.TP/tr.Entry-1) - 0.06
	if math.Abs(tr.PnLUSD-want) > 1e-9 || math.Abs(r.CostUSD-0.06) > 1e-9 {
		t.Fatalf("pnl %.4f want %.4f (cost %.4f)", tr.PnLUSD, want, r.CostUSD)
	}
}

// In a falling market the pullback strategy stays out (the trend is not rising).
func TestPullbackSkipsDowntrends(t *testing.T) {
	var p []float64
	for i := 0; i < 300; i++ {
		p = append(p, 2000-float64(i)*0.8)
	}
	if sig := (Pullback{Window: 20, DipPct: 0.3, Trend: 60}).Decide(p); sig != nil {
		t.Fatalf("bought a dip in a downtrend: %+v", sig)
	}
}

// The stop sells at the price that crossed it, not at the stop level.
func TestBacktestStopUsesTheCrossingPrice(t *testing.T) {
	p := []float64{100, 100, 99, 98, 97} // contrarian streak 2 fires at index 3 (98)
	s, _, _ := BuildStrategy(StrategyParams{Kind: "contrarian", Streak: 2, TPPct: 2, SLPct: 0.5, SizePct: 50})
	r := Backtest(s, p, BacktestOptions{SizeUSD: 10, Every: 1})
	if len(r.Trades) != 1 || r.Trades[0].Why != "sl" || r.Trades[0].Exit != 97 {
		t.Fatalf("want a stop at 97, got %+v", r.Trades)
	}
}

func TestTapeKeepsFineThenCoarseAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	tp := NewTape(path)
	start := time.Now().Add(-8 * time.Hour)
	for i := 0; i < 8*240; i++ { // 8 hours at 15s
		tp.Add("weth", start.Add(time.Duration(i)*15*time.Second), 2000+float64(i%10))
	}
	all := tp.Range("WETH", start, time.Now(), 0)
	// 2 old hours at one a minute + 6 recent hours at every 15s.
	if len(all) < 6*240 || len(all) > 6*240+2*60+5 {
		t.Fatalf("points %d", len(all))
	}
	if err := tp.Save(); err != nil {
		t.Fatal(err)
	}
	again := NewTape(path)
	if got := again.Range("WETH", start, time.Now(), 100); len(got) != 100 || got[len(got)-1] != all[len(all)-1] {
		t.Fatalf("reloaded %d points, last %+v", len(got), got[len(got)-1])
	}
}

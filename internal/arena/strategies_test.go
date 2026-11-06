package arena

import "testing"

func TestStrategies(t *testing.T) {
	flat := make([]float64, 30)
	for i := range flat {
		flat[i] = 3000
	}
	if (TrendHunter{5, 20}).Decide(flat) != nil {
		t.Fatal("trend must not fire on a flat market")
	}
	up := append(append([]float64{}, flat...), 3100)
	if (TrendHunter{5, 20}).Decide(up) == nil {
		t.Fatal("trend should fire on a cross up")
	}
	dip := append(append([]float64{}, flat...), 2940)
	if (DipBuyer{20, 1.5}).Decide(dip) == nil {
		t.Fatal("dip buyer should fire on a 2% dip")
	}
	down := append(append([]float64{}, flat...), 2990, 2980, 2970, 2960)
	if (Contrarian{4}).Decide(down) == nil {
		t.Fatal("contrarian should fire after 4 red ticks")
	}
}

// A person's agent: their settings are checked against the arena's limits,
// defaults are filled in, and their TP / SL / size replace the strategy's.
func TestBuildStrategyForPeople(t *testing.T) {
	bad := []StrategyParams{
		{Kind: "moon", TPPct: 2, SLPct: 1, SizePct: 20},
		{Kind: "contrarian", Streak: 1, TPPct: 2, SLPct: 1, SizePct: 20},
		{Kind: "trend", Fast: 20, Slow: 10, TPPct: 2, SLPct: 1, SizePct: 20},
		{Kind: "dip", DipPct: 50, TPPct: 2, SLPct: 1, SizePct: 20},
		{Kind: "contrarian", TPPct: 2, SLPct: 6, SizePct: 20}, // stop loss over 5%
		{Kind: "contrarian", TPPct: 0.1, SLPct: 1, SizePct: 20},
		{Kind: "contrarian", TPPct: 2, SLPct: 1, SizePct: 0},
	}
	for _, p := range bad {
		if _, _, err := BuildStrategy(p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	s, got, err := BuildStrategy(StrategyParams{Kind: "contrarian", Streak: 2, TPPct: 1.2, SLPct: 0.8, SizePct: 50})
	if err != nil || got.Streak != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	sig := s.Decide([]float64{3000, 2990, 2980})
	if sig == nil || sig.TPPct != 1.2 || sig.SLPct != 0.8 || sig.SizePct != 50 {
		t.Fatalf("tuned signal %+v", sig)
	}
	if _, def, _ := BuildStrategy(StrategyParams{Kind: "trend", TPPct: 2, SLPct: 1, SizePct: 20}); def.Fast != 5 || def.Slow != 20 {
		t.Fatalf("trend defaults %+v", def)
	}
}

// Settings a strategy does not use are dropped, never stored or shown.
func TestBuildStrategyDropsUnusedSettings(t *testing.T) {
	_, p, err := BuildStrategy(StrategyParams{Kind: "dip", Fast: -99, Slow: 999999, Streak: -3, Trend: 7, TPPct: 2, SLPct: 1, SizePct: 20})
	if err != nil {
		t.Fatal(err)
	}
	if p.Fast != 0 || p.Slow != 0 || p.Streak != 0 || p.Trend != 0 || p.Window != 20 || p.DipPct != 1.5 {
		t.Fatalf("stored %+v", p)
	}
}

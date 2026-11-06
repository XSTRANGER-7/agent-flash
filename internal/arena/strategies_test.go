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

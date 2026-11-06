package leaderboard

import (
	"math"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/store"
)

func series(v ...float64) []store.EquityPoint {
	out := make([]store.EquityPoint, len(v))
	for i, x := range v {
		out[i] = store.EquityPoint{At: time.Unix(int64(i), 0), USD: x}
	}
	return out
}

func TestSmartBeatsReckless(t *testing.T) {
	_, r1, d1 := Stats(series(100, 104, 100, 112)) // +12%, ~3.8% dd
	_, r2, d2 := Stats(series(100, 130, 91, 120))  // +20%, 30% dd
	s1, s2 := Score(r1, d1), Score(r2, d2)
	if math.Abs(r1-12) > 1e-9 || math.Abs(d2-30) > 1e-9 {
		t.Fatalf("stats: r1=%v d2=%v", r1, d2)
	}
	rows := Rank([]Row{{AgentID: "yolo", Score: s2, ReturnPct: r2}, {AgentID: "smart", Score: s1, ReturnPct: r1}})
	if rows[0].AgentID != "smart" || rows[0].Rank != 1 {
		t.Fatalf("ranking: %+v", rows)
	}
}

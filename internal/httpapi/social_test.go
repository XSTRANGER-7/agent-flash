package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/store"
)

func TestPublicBaseHonoursTheProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/s/leaderboard", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "demo.trycloudflare.com")
	if got := publicBase(r); got != "https://demo.trycloudflare.com" {
		t.Fatalf("base %s", got)
	}
	if money(-1.5) != "-$1.50" || money(75616) != "$75616" {
		t.Fatalf("money %s %s", money(-1.5), money(75616))
	}
}

// A test trade on a person's account once showed "Copy of TrendHunter" on its
// share card; the name must come from the trade's kind.
func TestTradeNamesFollowTheKind(t *testing.T) {
	s := &Server{E: &engine.Engine{Agents: map[string]arena.Profile{"trendhunter": {ID: "trendhunter", Name: "TrendHunter"}}}}
	cases := map[string]store.Trade{
		"TrendHunter":               {AgentID: "trendhunter", WalletID: "trendhunter", Kind: "agent"},
		"Test trade":                {AgentID: "trendhunter", WalletID: "u-1", Kind: "test"},
		"Manual order":              {AgentID: "trendhunter", WalletID: "u-1", Kind: "manual"},
		"Own wallet":                {AgentID: "trendhunter", WalletID: "self-1", Kind: "self"},
		"Invested with TrendHunter": {AgentID: "trendhunter", WalletID: "u-1", Kind: "invest"},
		"Copy of TrendHunter":       {AgentID: "trendhunter", WalletID: "follower1", Kind: "copy", CopyOf: "trd_x"},
	}
	for want, tr := range cases {
		if got := s.name(tr); got != want {
			t.Fatalf("%+v: got %q, want %q", tr, got, want)
		}
	}
}

// Losses must round like gains: a first real trade lost $0.046 and the proof
// page showed -$0.04 because the old round2 truncated toward zero.
func TestRound2IsSymmetric(t *testing.T) {
	for in, want := range map[float64]float64{-0.046057: -0.05, -0.05: -0.05, 0.046057: 0.05, 5.95: 5.95, -1.004: -1, 0: 0} {
		if got := round2(in); got != want {
			t.Fatalf("round2(%v) = %v, want %v", in, got, want)
		}
	}
}

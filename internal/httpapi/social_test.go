package httpapi

import (
	"net/http/httptest"
	"testing"
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

// Losses must round like gains: a first real trade lost $0.046 and the proof
// page showed -$0.04 because the old round2 truncated toward zero.
func TestRound2IsSymmetric(t *testing.T) {
	for in, want := range map[float64]float64{-0.046057: -0.05, -0.05: -0.05, 0.046057: 0.05, 5.95: 5.95, -1.004: -1, 0: 0} {
		if got := round2(in); got != want {
			t.Fatalf("round2(%v) = %v, want %v", in, got, want)
		}
	}
}

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

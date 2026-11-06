package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/evm"
)

func post(h http.HandlerFunc, ip string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/v1/me/x", bytes.NewReader(b))
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func resetUsers() {
	users.Lock()
	users.nonces, users.sessions, users.hits = map[string]userNonce{}, map[string]userSession{}, map[string][]time.Time{}
	users.deposits = map[string]pendingDeposit{}
	users.Unlock()
}

// A stranger asking for a sign-in message for someone else's address, or
// sending a bad signature for it, must not stop the owner from signing in.
func TestStrangerCannotBlockASignIn(t *testing.T) {
	resetUsers()
	s := &Server{E: &engine.Engine{Cfg: &config.Config{DataDir: t.TempDir()}}}
	key, _, _ := evm.GenerateKey()
	addr := key.Address()

	_, mine := post(s.meNonce, "198.51.100.1", map[string]any{"address": addr})
	for i := 0; i < 3; i++ { // the stranger's own messages for the same address
		post(s.meNonce, "203.0.113.7", map[string]any{"address": addr})
	}
	bad := "0x" + strings.Repeat("11", 65)
	if c, _ := post(s.meLogin, "203.0.113.7", map[string]any{"address": addr, "signature": bad, "nonce": mine["nonce"]}); c != 401 {
		t.Fatalf("bad signature: HTTP %d", c)
	}
	if c, _ := post(s.meLogin, "203.0.113.7", map[string]any{"address": addr, "signature": bad}); c != 401 {
		t.Fatalf("bad signature, no nonce: HTTP %d", c)
	}
	sig, _ := key.Sign(evm.PersonalMessageHash([]byte(mine["message"].(string))))
	body := map[string]any{"address": addr, "signature": evm.SigHex(sig), "nonce": mine["nonce"]}
	if c, out := post(s.meLogin, "198.51.100.1", body); c != 200 || !strings.HasPrefix(out["token"].(string), "u_") {
		t.Fatalf("owner sign-in after the stranger: HTTP %d %v", c, out)
	}
	if c, _ := post(s.meLogin, "198.51.100.1", body); c != 401 {
		t.Fatalf("a used message signed in again: HTTP %d", c)
	}
	// An older app sends no nonce: the newest messages for the address are tried.
	_, again := post(s.meNonce, "198.51.100.1", map[string]any{"address": addr})
	post(s.meNonce, "203.0.113.7", map[string]any{"address": addr})
	sig, _ = key.Sign(evm.PersonalMessageHash([]byte(again["message"].(string))))
	if c, _ := post(s.meLogin, "198.51.100.1", map[string]any{"address": addr, "signature": evm.SigHex(sig)}); c != 200 {
		t.Fatalf("sign-in without a nonce: HTTP %d", c)
	}
}

// Someone else's try at a pending deposit is refused and does not use it up.
func TestWrongOwnerDoesNotSpendADeposit(t *testing.T) {
	resetUsers()
	s := &Server{E: &engine.Engine{Cfg: &config.Config{DataDir: t.TempDir()}}}
	owner := "0x000000000000000000000000000000000000dEaD"
	users.Lock()
	users.deposits["0xabc"] = pendingDeposit{auth: engine.DepositAuth{From: owner}, walletID: "u-x", exp: time.Now().Add(time.Minute)}
	users.Unlock()
	b, _ := json.Marshal(map[string]any{"nonce": "0xabc", "signature": "0x00"})
	w := httptest.NewRecorder()
	s.meDeposit(w, httptest.NewRequest("POST", "/v1/me/deposit", bytes.NewReader(b)), "0x000000000000000000000000000000000000bEEF")
	users.Lock()
	_, still := users.deposits["0xabc"]
	users.Unlock()
	if w.Code != 403 || !still {
		t.Fatalf("HTTP %d, pending kept %v", w.Code, still)
	}
}

// Rate-limit buckets nobody used for an hour are dropped once there are many.
func TestRateLimitBucketsAreSwept(t *testing.T) {
	resetUsers()
	old := []time.Time{time.Now().Add(-2 * time.Hour)}
	users.Lock()
	for i := 0; i <= maxHitBuckets; i++ {
		users.hits["x|"+strings.Repeat("a", i%7)+string(rune('a'+i%26))+time.Duration(i).String()] = old
	}
	users.Unlock()
	r := httptest.NewRequest("GET", "/", nil)
	if !allow("t", r, 5, time.Minute) {
		t.Fatal("refused")
	}
	users.Lock()
	n := len(users.hits)
	users.Unlock()
	if n != 1 {
		t.Fatalf("%d buckets left, want 1", n)
	}
}

// A forwarded client IP counts only when it comes from this machine.
func TestForwardedIPOnlyFromLocalProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
	if clientIP(r) != "203.0.113.9" {
		t.Fatalf("forged header trusted: %s", clientIP(r))
	}
	r.RemoteAddr = "127.0.0.1:4000"
	if clientIP(r) != "1.2.3.4" {
		t.Fatalf("tunnel header ignored: %s", clientIP(r))
	}
}

func TestAgentNameAndTaglineChecks(t *testing.T) {
	if nameKey("Trend Hunter") != nameKey("TrendHunter") || nameKey("trend-hunter") != "trendhunter" {
		t.Fatal("look-alike names are not caught")
	}
	for _, bad := range []string{"hi\x00", "abc‮evil", "zero​width", "line\nbreak"} {
		if strings.IndexFunc(bad, hiddenRune) < 0 {
			t.Fatalf("%q passed", bad)
		}
	}
	if strings.IndexFunc("Buys dips 📉 then sells 🚀, ça va", hiddenRune) >= 0 {
		t.Fatal("normal text refused")
	}
}

// Unknown API routes and wrong methods answer in JSON, like every API error.
func TestUnknownAPIRoutesAnswerJSON(t *testing.T) {
	s := New(&engine.Engine{Cfg: &config.Config{DataDir: t.TempDir()}}, slogDiscard())
	h := s.Handler()
	for _, c := range []struct {
		method, path string
		code         int
	}{{"POST", "/v1/nope", 404}, {"GET", "/v1/nope", 404}, {"DELETE", "/v1/trades", 405}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.code || !strings.Contains(w.Header().Get("Content-Type"), "json") {
			t.Errorf("%s %s: HTTP %d %q", c.method, c.path, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// e2e drives a running Agent Arena server in PAPER mode through every
// feature, the way a user would (HTTP API only), and prints PASS / FAIL:
// small and large trades, risk rules, copy trading with human approval,
// investing, AI auto-trade plans, emergency stop, withdrawals and agent
// transparency. Quotes, prices and signatures are the real Flash API's; the
// money is paper.
//
//	go run ./cmd/e2e                      (server on http://127.0.0.1:8080)
//	go run ./cmd/e2e -base http://host:port -env .env
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/evm"
)

var (
	base   = flag.String("base", "http://127.0.0.1:8080", "server URL")
	envF   = flag.String("env", ".env", "env file holding ADMIN_TOKEN (never printed)")
	token  string
	runTag = time.Now().Format("150405")
	invID  string // this run's investor sub-wallet
	fails  int
	total  int
	// QA helper: print a fresh signed-in account (paper only) and exit.
	newAccount = flag.Float64("new-account", -1, "create a signed-in paper account funded with this many USDC by signature, print {addr, token, exp, id} as JSON and exit")
)

type M = map[string]any

func main() {
	flag.Parse()
	token = readToken(*envF)
	if token == "" {
		fmt.Println("ADMIN_TOKEN not found in", *envF)
		os.Exit(2)
	}
	st := get("/v1/flash/status")
	if st["mode"] != "paper" {
		fmt.Printf("server mode is %v; e2e only runs against MODE=paper (it places many orders)\n", st["mode"])
		os.Exit(2)
	}
	if *newAccount >= 0 {
		key, _, _ := evm.GenerateKey()
		tok, _ := signIn(key)
		_, me := meDo("POST", "/v1/me/account", M{}, tok)
		if *newAccount > 0 {
			_, p := meDo("POST", "/v1/me/deposit/prepare", M{"amountUsd": *newAccount}, tok)
			meDo("POST", "/v1/me/deposit", M{"nonce": s(p, "nonce"), "signature": signTyped(key, s(p, "typedData"))}, tok)
		}
		out, _ := json.Marshal(M{"addr": key.Address(), "token": tok, "exp": time.Now().Add(7 * 24 * time.Hour).UTC(), "id": s(me, "account", "id")})
		fmt.Println(string(out))
		return
	}
	start := time.Now()
	section("1. Transparency and public data")
	transparency(st)
	section("2. Small trade ($1 test trade): fill, bracket, exit")
	// A fresh sub-wallet per run, so earlier runs (daily trade limit,
	// leftover balances) never change the result.
	_, nw := post("/v1/admin/wallets", M{"owner": "E2E " + runTag + " Trader", "agentId": "trendhunter", "mode": "copy",
		"payout": "0x000000000000000000000000000000000000dEaD", "maxPerTradeUsd": 1, "dailyCapUsd": 1})
	w := s(nw, "wallet", "id")
	if !check("fresh trader sub-wallet", w != "", errMsg(nw)) {
		os.Exit(1)
	}
	smallTrade(w)
	section("3. Risk management")
	risk(w)
	section("4. Large trade ($100, request $150 is capped)")
	largeTrade(w)
	section("5. Copy trading + human approval + invest")
	copyAndInvest()
	section("6. AI automated trading (agents + personal plans)")
	automation()
	section("7. Emergency stop and withdrawals")
	safetyOps(w, "follower2")
	section("8. Flash advanced order types (limit, DCA/TWAP, stop buy, trailing, SL + TP)")
	orderTypes()
	section("9. Social: share cards, signals, RSS, fees")
	social()
	section("10. Your own wallet (non-custodial signing)")
	selfCustody()
	section("11. Gasless account (sign in, add USDC by signature, invest, withdraw)")
	gaslessAccount()
	section("12. Community agents (anyone makes an agent; others watch, invest, copy)")
	communityAgents()
	fmt.Printf("\n%d checks, %d failed, %s\n", total, fails, time.Since(start).Round(time.Second))
	if fails > 0 {
		os.Exit(1)
	}
}

// --- tiny test harness ---------------------------------------------------------

func section(s string) { fmt.Println("\n" + s) }

func check(name string, ok bool, detail string) bool {
	total++
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		fails++
	}
	if len(detail) > 120 {
		detail = detail[:120] + "…"
	}
	fmt.Printf("  %s  %-44s %s\n", mark, name, detail)
	return ok
}

func readToken(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "ADMIN_TOKEN=") {
			return strings.Trim(strings.TrimPrefix(l, "ADMIN_TOKEN="), `"'`)
		}
	}
	return ""
}

func do(method, path string, body any, admin bool) (int, M, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, *base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{Timeout: 3 * time.Minute}
	res, err := c.Do(req)
	if err != nil {
		return 0, M{}, err.Error()
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m M
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = M{}
	}
	return res.StatusCode, m, string(raw)
}

func get(p string) M  { _, m, _ := do("GET", p, nil, false); return m }
func aget(p string) M { _, m, _ := do("GET", p, nil, true); return m }
func post(p string, b any) (int, M) {
	c, m, _ := do("POST", p, b, true)
	return c, m
}

func path(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(M)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}
func s(m any, keys ...string) string { v, _ := path(m, keys...).(string); return v }
func f(m any, keys ...string) float64 {
	v, _ := path(m, keys...).(float64)
	return v
}
func list(m any, keys ...string) []any { v, _ := path(m, keys...).([]any); return v }

// errMsg reads the API error ({"error":{"message":...}} or {"error":"..."}).
func errMsg(m M) string {
	if e := s(m, "error", "message"); e != "" {
		return e
	}
	return s(m, "error")
}

func price() float64 { return f(get("/v1/price"), "last") }

// waitTrade polls a trade until pred holds (or 60s).
func waitTrade(id string, pred func(M) bool) M {
	var t M
	for i := 0; i < 40; i++ {
		t, _ = get("/v1/trades/" + id)["trade"].(M)
		if t != nil && pred(t) {
			return t
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return t
}

func filledActive(t M) bool { return t["status"] == "filled" && t["protection"] == "active" }
func closed(t M) bool       { return t["status"] == "closed" }

func steps(t M) string {
	var out []string
	for _, x := range list(t, "timeline") {
		out = append(out, s(x, "step"))
	}
	return strings.Join(out, ",")
}

// openTradeOf returns the id of a wallet's open trade, if any.
func openTradeOf(wallet string) string {
	for _, x := range list(get("/v1/trades?limit=200"), "trades") {
		if s(x, "walletId") == wallet {
			switch s(x, "status") {
			case "placed", "filled", "unprotected":
				return s(x, "id")
			}
		}
	}
	return ""
}

// freeWallet exits a wallet's open position so it can trade again.
func freeWallet(wallet string) {
	if id := openTradeOf(wallet); id != "" {
		t := waitTrade(id, func(t M) bool { return t["status"] != "placed" })
		if t["status"] == "filled" || t["status"] == "unprotected" {
			post("/v1/admin/trades/"+id+"/exit", nil)
			waitTrade(id, closed)
		}
	}
}

func propose(wallet, agent string, size, tpPct, slPct float64, extra M) (int, M) {
	px := price()
	body := M{"agentId": agent, "walletId": wallet, "sizeUsd": size, "takeProfit": round2(px * (1 + tpPct/100)),
		"stopLoss": round2(px * (1 - slPct/100)), "reason": "e2e test"}
	for k, v := range extra {
		body[k] = v
	}
	return post("/v1/admin/propose", body)
}

func round2(x float64) float64 { return float64(int64(x*100+0.5)) / 100 }

// --- 1 -----------------------------------------------------------------------

func transparency(st M) {
	check("mode is paper", st["mode"] == "paper", "real Flash quotes, simulated fills")
	check("Flash WebSocket live", st["stream"] == "live", fmt.Sprint(st["stream"]))
	lq, _ := st["liveQuote"].(M)
	check("live Flash quote streaming", lq != nil && f(lq, "weth") > 0,
		fmt.Sprintf("$%.0f buys %.6f WETH, all-in %.2f%%", f(lq, "usd"), f(lq, "weth"), f(lq, "costPct")))
	toks := list(get("/v1/tokens"), "tokens")
	var tk []string
	okT := len(toks) >= 2
	for _, t := range toks {
		tk = append(tk, fmt.Sprintf("%s $%.2f (%s)", s(t, "symbol"), f(t, "price"), s(t, "source")))
		okT = okT && f(t, "price") > 0
	}
	check("coins priced (WETH pool, cbBTC Flash)", okT, strings.Join(tk, ", "))
	lb := get("/v1/leaderboard")
	check("leaderboard formula is public", strings.Contains(s(lb, "formula"), "MaxDrawdown"), s(lb, "formula"))
	check("3 agents ranked", len(list(lb, "rows")) == 3, "")
	ag := list(get("/v1/agents"), "agents")
	okA := len(ag) >= 3 // the 3 built-in agents, plus any community agents
	for _, a := range ag {
		okA = okA && strings.HasPrefix(s(a, "address"), "0x")
	}
	check("agent wallets public (verifiable)", okA, "every agent shows its Base address")
	pf := get("/v1/proof")
	nl, _ := pf["notListed"].(M)
	check("proof page lists no paper trades", len(list(pf, "trades")) == 0,
		fmt.Sprintf("%d real listed, %.0f paper only counted", len(list(pf, "trades")), f(nl, "paper")))
	c, _, _ := do("GET", "/v1/admin/wallets", nil, false)
	check("admin API needs the token", c == 401, fmt.Sprint("HTTP ", c))
	leak := false
	for _, p := range []string{"/v1/flash/status", "/v1/agents", "/v1/feed?limit=100", "/v1/trades?limit=100", "/v1/plans", "/v1/stats", "/v1/proof"} {
		_, _, raw := do("GET", p, nil, false)
		if strings.Contains(raw, "dpka_") || strings.Contains(raw, token) {
			leak = true
		}
	}
	check("no keys or tokens in public data", !leak, "status, agents, feed, trades, plans, stats")
}

// --- 2 -----------------------------------------------------------------------

var smallCost float64

func smallTrade(w string) {
	post("/v1/admin/wallets/"+w+"/resume", nil)
	freeWallet(w)
	post("/v1/admin/wallets/"+w+"/topup", M{"usd": 25})
	code, r := post("/v1/admin/test-trade", M{"walletId": w})
	tr, _ := r["trade"].(M)
	guard := strings.Contains(s(r, "error"), "cost guard")
	check("$1 test trade: placed, or refused for cost", (code == 200 && tr != nil) || guard, fmt.Sprintf("HTTP %d %s", code, s(r, "error")))
	if guard {
		// Flash's fixed cost is high right now: the guard refused $1, as designed.
		fmt.Sscanf(s(r, "error")[strings.Index(s(r, "error"), "(")+1:], "%f", &smallCost)
		code, r = propose(w, "trendhunter", 10, 2, 1, nil)
		tr, _ = r["trade"].(M)
		if !check("small trade ($10) placed instead", code == 200 && tr != nil, fmt.Sprintf("HTTP %d %s", code, s(r, "error"))) {
			return
		}
	} else if tr == nil {
		return
	} else {
		smallCost = f(tr, "entryCostPct")
	}
	id := s(tr, "id")
	check("real Flash quote on the receipt", f(tr, "markPrice") > 0 && f(tr, "entryCostPct") > 0,
		fmt.Sprintf("$%.0f: mark $%.2f, all-in cost %.2f%% ($%.3f)", f(tr, "sizeUsd"), f(tr, "markPrice"), f(tr, "entryCostPct"), f(tr, "entryCostUsd")))
	t := waitTrade(id, filledActive)
	check("filled, bracket active", filledActive(t), fmt.Sprintf("%s / %s, bought %.8f WETH", s(t, "status"), s(t, "protection"), f(t, "wethAmount")))
	c2, r2 := propose(w, "trendhunter", 1, 2, 1, nil)
	check("second trade blocked (one position)", c2 == 422 && s(r2, "decision", "code") == "OPEN_POSITION", s(r2, "decision", "code"))
	post("/v1/admin/trades/"+id+"/exit", nil)
	t = waitTrade(id, closed)
	pnl := path(t, "pnlUsd")
	check("exit now: closed with P&L from fills", closed(t) && pnl != nil, fmt.Sprintf("P&L $%v, received $%.4f", pnl, f(t, "usdcReceived")))
	check("receipt timeline complete", strings.Contains(steps(t), "quoted") && strings.Contains(steps(t), "signed") &&
		strings.Contains(steps(t), "filled") && strings.Contains(steps(t), "closed"), steps(t))
	ords := list(aget("/v1/admin/wallets/"+w+"/orders"), "orders")
	var o []string
	for _, x := range ords[:min(3, len(ords))] {
		o = append(o, s(x, "Type")+"/"+s(x, "Side")+" "+s(x, "Status"))
	}
	check("Flash order history (GET /orders)", len(ords) >= 3, strings.Join(o, ", "))
}

// --- 3 -----------------------------------------------------------------------

func largeTrade(w string) {
	freeWallet(w)
	post("/v1/admin/wallets/"+w+"/topup", M{"usd": 400})
	code, r := propose(w, "trendhunter", 150, 2, 1, nil)
	tr, _ := r["trade"].(M)
	if !check("large trade placed", code == 200 && tr != nil, fmt.Sprintf("HTTP %d %v %v", code, path(r, "decision", "reason"), r["error"])) {
		return
	}
	check("$150 request capped to $100 (max trade)", f(tr, "sizeUsd") == 100, fmt.Sprintf("$%.2f", f(tr, "sizeUsd")))
	lc := f(tr, "entryCostPct")
	check("bigger order costs less in % (real quotes)", lc > 0 && lc < smallCost, fmt.Sprintf("$100: %.2f%% vs $1: %.2f%%", lc, smallCost))
	t := waitTrade(s(tr, "id"), filledActive)
	check("$100 filled, bracket active", filledActive(t), fmt.Sprintf("bought %.6f WETH at $%.2f", f(t, "wethAmount"), f(t, "fillPrice")))
	post("/v1/admin/trades/"+s(tr, "id")+"/exit", nil)
	t = waitTrade(s(tr, "id"), closed)
	check("$100 position exited", closed(t), fmt.Sprintf("P&L $%v", path(t, "pnlUsd")))
}

// --- 4 -----------------------------------------------------------------------

func risk(w string) {
	freeWallet(w)
	px := price()
	cases := []struct {
		name, code string
		body       M
	}{
		{"no stop loss", "STOP_LOSS", M{"stopLoss": 0}},
		{"stop loss 10% away", "RISK", M{"stopLoss": round2(px * 0.9)}},
		{"take profit too close", "TAKE_PROFIT", M{"takeProfit": round2(px * 1.001)}},
		{"coin not on the allowlist", "TOKEN", M{"token": "DOGE"}},
	}
	for _, c := range cases {
		code, r := propose(w, "trendhunter", 5, 2, 1, c.body)
		check("blocks: "+c.name, code == 422 && s(r, "decision", "code") == c.code, s(r, "decision", "code")+" "+s(r, "decision", "reason"))
	}
	// A fresh sub-wallet ($25 paper) for the position-size rule.
	_, nw := post("/v1/admin/wallets", M{"owner": "E2E " + runTag + " Risk", "agentId": "trendhunter", "mode": "copy", "payout": "0x000000000000000000000000000000000000dEaD", "maxPerTradeUsd": 1, "dailyCapUsd": 1})
	bal := f(nw, "wallet", "usdc")
	if fresh := s(nw, "wallet", "id"); check("fresh wallet for the size rule", fresh != "" && bal > 0, fmt.Sprintf("$%.0f paper USDC %s", bal, errMsg(nw))) {
		// Bet 15 points over the server's MAX_POSITION_PCT (25 by default).
		maxPos := f(get("/v1/stats"), "limits", "maxPositionPct")
		if maxPos <= 0 {
			maxPos = 25
		}
		bet := math.Min(maxPos+15, 95)
		code, r := propose(fresh, "trendhunter", float64(int(bal*bet/100)), 2, 1, nil)
		check(fmt.Sprintf("blocks: bet %.0f%% of a $%.0f wallet (max %.0f%%)", bet, bal, maxPos), code == 422 && s(r, "decision", "code") == "SIZE", s(r, "decision", "code")+" "+s(r, "decision", "reason"))
	}
	post("/v1/admin/wallets/"+w+"/pause", nil)
	code, r := propose(w, "trendhunter", 5, 2, 1, nil)
	check("blocks: paused wallet", code == 422 && s(r, "decision", "code") == "WALLET_PAUSED", s(r, "decision", "code"))
	post("/v1/admin/wallets/"+w+"/resume", nil)
	bad := []struct {
		name string
		mut  M
	}{
		{"plan stop loss 10%", M{"stopLossPct": 10}},
		{"plan slippage 9%", M{"maxSlippagePct": 9}},
		{"plan $0.50 per trade", M{"tradeUsd": 0.5}},
		{"plan payout not an address", M{"payout": "me"}},
	}
	for _, b := range bad {
		body := planBody("e2e invalid", "WETH", "interval")
		for k, v := range b.mut {
			body[k] = v
		}
		code, r := post("/v1/admin/plans", body)
		check("rejects: "+b.name, code == 400, errMsg(r))
	}
}

// --- 5 -----------------------------------------------------------------------

func copyAndInvest() {
	// pick the agent with no open position (exit the oldest if all are busy)
	agent := ""
	for _, a := range []string{"dipbuyer", "contrarian", "trendhunter"} {
		if openTradeOf(a) == "" {
			agent = a
			break
		}
	}
	if agent == "" {
		agent = "dipbuyer"
		freeWallet(agent)
	}
	post("/v1/admin/wallets/follower1/resume", nil)
	freeWallet("follower1")
	post("/v1/admin/wallets/follower1/topup", M{"usd": 300})
	_, wr, _ := do("PATCH", "/v1/admin/wallets/follower1", M{"agentId": agent, "mode": "copy", "allocationPct": 25, "maxPerTradeUsd": 60, "dailyCapUsd": 500}, true)
	check("follower1 copies "+agent, wr["wallet"] != nil, fmt.Sprint(wr["error"]))
	code, inv := post("/v1/admin/wallets", M{"owner": "E2E " + runTag + " Inv", "agentId": agent, "mode": "invest", "payout": "0x000000000000000000000000000000000000dEaD",
		"allocationPct": 20, "maxPerTradeUsd": 20, "dailyCapUsd": 60, "lossLimitPct": 15})
	if invID0 := s(inv, "wallet", "id"); invID0 != "" {
		post("/v1/admin/wallets/"+invID0+"/topup", M{"usd": 75}) // $100: 20% = $20, cheap enough on Flash
	}
	invID = s(inv, "wallet", "id")
	check("investor sub-wallet created", code == 201 && invID != "", s(inv, "wallet", "address")+errMsg(inv))

	// $20 keeps Flash's fixed cost under the 2.5% cost guard even when gas is up
	post("/v1/admin/wallets/"+agent+"/topup", M{"usd": 100})
	code, r := propose(agent, agent, 20, 2, 1, nil)
	at, _ := r["trade"].(M)
	if !check("agent trade placed", code == 200 && at != nil, fmt.Sprintf("HTTP %d %v %v", code, path(r, "decision", "reason"), r["error"])) {
		return
	}
	// the $60 copy is above the paper approval threshold ($50): a human decides
	var appr M
	for i := 0; i < 30 && appr == nil; i++ {
		for _, a := range list(aget("/v1/admin/approvals"), "approvals") {
			if s(a, "walletId") == "follower1" && s(a, "status") == "pending" {
				appr, _ = a.(M)
			}
		}
		time.Sleep(time.Second)
	}
	if check("big copy waits for human approval", appr != nil, fmt.Sprintf("$%.2f: %s", f(appr, "amountUsd"), s(appr, "summary"))) {
		c, _ := post("/v1/admin/approvals/"+s(appr, "id")+"/approve", nil)
		check("approved by the operator", c == 200, "")
	}
	var cp, it M
	for i := 0; i < 40 && (cp == nil || it == nil); i++ {
		for _, x := range list(get("/v1/trades?limit=50"), "trades") {
			if s(x, "copyOf") == s(at, "id") {
				cp, _ = x.(M)
			}
			if s(x, "walletId") == invID && s(x, "kind") == "invest" {
				it, _ = x.(M)
			}
		}
		time.Sleep(time.Second)
	}
	check("copy placed with the agent's stop loss", cp != nil && f(cp, "stopLoss") == f(at, "stopLoss") && f(cp, "sizeUsd") == 60,
		fmt.Sprintf("$%.2f, SL %.2f (agent SL %.2f)", f(cp, "sizeUsd"), f(cp, "stopLoss"), f(at, "stopLoss")))
	check("investor wallet traded on the signal", it != nil, fmt.Sprintf("$%.2f by %s", f(it, "sizeUsd"), agent))
	if cp != nil {
		t := waitTrade(s(cp, "id"), filledActive)
		check("copy filled, bracket active", filledActive(t), s(t, "status")+" / "+s(t, "protection"))
	}
}

// --- 6 -----------------------------------------------------------------------

func planBody(name, token, strategy string) M {
	return M{"name": name, "owner": "E2E", "payout": "0x000000000000000000000000000000000000dEaD", "token": token, "strategy": strategy,
		"intervalMin": 1, "tradeUsd": 10, "budgetUsd": 30, "takeProfitPct": 1.5, "stopLossPct": 1, "maxSlippagePct": 1,
		"maxTradesPerDay": 5, "lossLimitPct": 50, "durationHours": 1, "exitAtEnd": true}
}

func automation() {
	n := 0
	for _, x := range list(get("/v1/trades?limit=200"), "trades") {
		if s(x, "kind") == "agent" && !strings.Contains(s(x, "reason"), "e2e") && !strings.Contains(s(x, "reason"), "operator") {
			n++
		}
	}
	if up := f(get("/readyz"), "uptimeSec"); n == 0 && up < 600 {
		fmt.Printf("  SKIP  %-44s %s\n", "agents trade on their own signals", fmt.Sprintf("server up %.0fs; strategies need ~5 min of price history", up))
	} else {
		check("agents trade on their own signals", n > 0, fmt.Sprintf("%d autonomous agent trades so far", n))
	}
	code, r := post("/v1/admin/plans", planBody("e2e cbBTC interval", "cbBTC", "interval"))
	id := s(r, "plan", "id")
	if check("plan created (cbBTC, every 1 min)", code == 201 && id != "", fmt.Sprintf("HTTP %d %v", code, r["error"])) {
		var p M
		for i := 0; i < 20; i++ {
			p = get("/v1/plans/" + id)
			if len(list(p, "trades")) > 0 {
				break
			}
			time.Sleep(1500 * time.Millisecond)
		}
		tr := list(p, "trades")
		var t M
		if len(tr) > 0 {
			t, _ = tr[0].(M)
		}
		if sig := s(p, "plan", "lastSignal"); t == nil && strings.Contains(sig, "cost guard") {
			// Flash's fixed cost at $10 moves through the day: a refusal by
			// the cost guard is the guard working, not a failure.
			fmt.Printf("  SKIP  %-44s %s\n", "plan bought cbBTC itself", sig)
		} else {
			check("plan bought cbBTC itself", t != nil && s(t, "token") == "cbBTC" && s(t, "kind") == "plan" && f(t, "sizeUsd") == 10,
				fmt.Sprintf("$%.0f %s, TP %.2f SL %.2f", f(t, "sizeUsd"), s(t, "token"), f(t, "takeProfit"), f(t, "stopLoss")))
		}
		if t != nil {
			entry := f(t, "entry")
			check("plan kept its own TP/SL", entry > 0 && abs(f(t, "takeProfit")/entry-1.015) < 0.002 && abs(f(t, "stopLoss")/entry-0.99) < 0.002,
				fmt.Sprintf("+%.2f%% / -%.2f%%", (f(t, "takeProfit")/entry-1)*100, (1-f(t, "stopLoss")/entry)*100))
		}
		check("plan explains what it is doing", s(p, "plan", "lastSignal") != "", s(p, "plan", "lastSignal"))
		c, pr := post("/v1/admin/plans/"+id+"/pause", nil)
		check("plan pause", c == 200 && s(pr, "plan", "status") == "paused", s(pr, "plan", "status"))
		c, pr = post("/v1/admin/plans/"+id+"/stop", nil)
		check("plan stop", c == 200 && s(pr, "plan", "status") == "stopped", s(pr, "plan", "status"))
	}
	code, r = post("/v1/admin/plans", planBody("e2e AI auto-pilot", "WETH", "auto"))
	id = s(r, "plan", "id")
	if check("AI auto-pilot plan created", code == 201 && id != "", fmt.Sprint(r["error"])) {
		_, pr := post("/v1/admin/plans/"+id+"/check", nil)
		sig := s(pr, "plan", "lastSignal")
		check("auto-pilot picks the best strategy", strings.Contains(sig, "auto-pilot picked") || strings.HasPrefix(sig, "entered"), sig)
		post("/v1/admin/plans/"+id+"/stop", nil)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// --- 7 -----------------------------------------------------------------------

func safetyOps(w, noPayout string) {
	c, r := post("/v1/admin/emergency-stop", nil)
	check("emergency stop", c == 200, fmt.Sprintf("cancelled %v pending orders", r["cancelled"]))
	st := get("/v1/flash/status")
	check("status shows halted", st["halted"] == true, "")
	code, rr := propose(w, "trendhunter", 5, 2, 1, nil)
	check("no new orders while halted", code == 422 && s(rr, "decision", "code") == "HALTED", s(rr, "decision", "code"))
	post("/v1/admin/resume", nil)
	check("trading resumed", get("/v1/flash/status")["halted"] == false, "")
	freeWallet(w)
	c, r = post("/v1/admin/wallets/"+noPayout+"/withdraw", M{"token": "USDC"})
	check("withdraw refused without a payout lock", c == 400 && strings.Contains(errMsg(r), "payout"), errMsg(r))
	inv := invID
	if check("investor wallet found", inv != "", inv) {
		freeWallet(inv)
		c, r = post("/v1/admin/wallets/"+inv+"/withdraw", M{"token": "USDC"})
		check("withdraw goes only to the locked payout", c == 200 && strings.EqualFold(s(r, "to"), "0x000000000000000000000000000000000000dEaD"),
			fmt.Sprintf("%v USDC to %s", r["amount"], s(r, "to")))
	}
}

// --- 8 -----------------------------------------------------------------------

func freshWallet(label string) string {
	_, nw := post("/v1/admin/wallets", M{"owner": "E2E " + runTag + " " + label, "agentId": "trendhunter", "mode": "copy",
		"payout": "0x000000000000000000000000000000000000dEaD", "maxPerTradeUsd": 1, "dailyCapUsd": 1})
	id := s(nw, "wallet", "id")
	post("/v1/admin/wallets/"+id+"/topup", M{"usd": 300})
	return id
}

func ticket(body M) (int, M) { return post("/v1/admin/orders", body) }

func orderTypes() {
	w := freshWallet("Orders")
	code, r := ticket(M{"walletId": w, "style": "limit", "sizeUsd": 20, "limitPct": 2, "takeProfitPct": 3, "stopLossPct": 1})
	lt, _ := r["trade"].(M)
	if check("limit buy rests below the price", code == 200 && lt != nil && s(lt, "orderType") == "limit+bracket", fmt.Sprintf("HTTP %d %s %s", code, s(r, "decision", "reason"), s(r, "error"))) {
		t := waitTrade(s(lt, "id"), func(t M) bool { return true })
		check("limit has not filled at the market", s(t, "status") == "placed", s(t, "status"))
		c, _ := post("/v1/admin/trades/"+s(lt, "id")+"/cancel", nil)
		t = waitTrade(s(lt, "id"), func(t M) bool { return s(t, "status") != "placed" })
		check("resting limit cancelled (signed cancel)", c == 200 && s(t, "status") == "cancelled", s(t, "status"))
	}
	w2 := freshWallet("DCA")
	// Flash charges its fixed cost on every slice ($0.02-0.19 each, it moves a
	// lot), so 5 slices of a $60 order stay under the 2.5% cost guard.
	code, r = ticket(M{"walletId": w2, "style": "twap", "sizeUsd": 60, "twapMinutes": 5, "takeProfitPct": 3, "stopLossPct": 1})
	tw, _ := r["trade"].(M)
	if check("DCA / TWAP order placed (5 slices)", code == 200 && tw != nil && s(tw, "orderType") == "twap+bracket", fmt.Sprintf("HTTP %d %s %s", code, s(r, "decision", "reason"), s(r, "error"))) {
		t := waitTrade(s(tw, "id"), func(t M) bool { return s(t, "bracketOrderId") != "" })
		check("first slice filled, bracket already active", s(t, "bracketOrderId") != "", "status "+s(t, "status"))
	}
	w3 := freshWallet("Stop")
	code, r = ticket(M{"walletId": w3, "style": "stop", "sizeUsd": 20, "stopPct": 1.5, "takeProfitPct": 3, "stopLossPct": 1, "protect": "trailing", "trailPct": 2})
	st, _ := r["trade"].(M)
	if check("stop buy with trailing stop placed", code == 200 && st != nil && s(st, "orderType") == "stop+protect" && s(st, "protectMode") == "trailing",
		fmt.Sprintf("HTTP %d %s %s", code, s(r, "decision", "reason"), s(r, "error"))) {
		check("stop buy waits for its trigger", f(st, "stopPrice") > price(), fmt.Sprintf("triggers at $%.2f (now $%.2f)", f(st, "stopPrice"), price()))
	}
	code, r = ticket(M{"walletId": w3, "style": "market", "sizeUsd": 20, "protect": "trailing", "trailPct": 2})
	check("rejects trailing on a market entry", code == 422 && s(r, "decision", "code") == "ORDER_TYPE", s(r, "decision", "reason"))
	code, r = ticket(M{"walletId": w3, "style": "twap", "sizeUsd": 20, "twapMinutes": 2})
	check("rejects DCA shorter than 5 minutes", code == 422, s(r, "decision", "reason"))
	w4 := freshWallet("SLTP")
	code, r = ticket(M{"walletId": w4, "style": "stop", "sizeUsd": 20, "stopPct": 1.5, "takeProfitPct": 3, "stopLossPct": 1, "protect": "separate"})
	sp, _ := r["trade"].(M)
	check("stop buy with separate SL + TP placed", code == 200 && sp != nil && s(sp, "orderType") == "stop+protect" && s(sp, "protectMode") == "separate",
		fmt.Sprintf("HTTP %d %s %s", code, s(r, "decision", "reason"), s(r, "error")))
	code, r = ticket(M{"walletId": w4, "style": "market", "sizeUsd": 20, "protect": "separate"})
	check("rejects separate SL + TP on a market entry", code == 422 && s(r, "decision", "code") == "ORDER_TYPE", s(r, "decision", "reason"))
}

// --- 9 -----------------------------------------------------------------------

func social() {
	sg := list(get("/v1/signals"), "signals")
	check("signals feed (agent trades)", len(sg) > 0, fmt.Sprintf("%d signals, latest %s %s", len(sg), s(firstM(sg), "agent"), s(firstM(sg), "token")))
	if len(sg) > 0 {
		_, _, raw := do("GET", "/s/trade/"+s(firstM(sg), "id"), nil, false)
		check("trade share page has a preview card", strings.Contains(raw, `property="og:title"`) && strings.Contains(raw, "twitter:card"), metaContent(raw, "og:title"))
	}
	_, _, raw := do("GET", "/s/leaderboard", nil, false)
	check("leaderboard share page", strings.Contains(raw, "og:description") && strings.Contains(raw, "#1"), metaContent(raw, "og:description"))
	_, _, raw = do("GET", "/s/agent/trendhunter", nil, false)
	check("agent share page", strings.Contains(raw, "TrendHunter"), metaContent(raw, "og:title"))
	_, _, raw = do("GET", "/signals.rss", nil, false)
	check("RSS feed", strings.Contains(raw, "<rss") && strings.Contains(raw, "<item>"), fmt.Sprintf("%d items", strings.Count(raw, "<item>")))
	st := get("/v1/stats")
	check("fees on every trade (monetization)", f(st, "fees", "bps") > 0 && f(st, "fees", "routedUsd") > 0,
		fmt.Sprintf("%.0f bps, $%.2f routed, $%.4f earned", f(st, "fees", "bps"), f(st, "fees", "routedUsd"), f(st, "fees", "earnedUsd")))
	// By now this run placed many paper trades: the proof page counts them, never lists them.
	pf := get("/v1/proof")
	nl, _ := pf["notListed"].(M)
	check("proof page counts paper trades, lists none", len(list(pf, "trades")) == 0 && f(nl, "paper") > 0,
		fmt.Sprintf("%d real listed, %.0f paper only counted", len(list(pf, "trades")), f(nl, "paper")))
}

func firstM(l []any) M {
	if len(l) == 0 {
		return M{}
	}
	m, _ := l[0].(M)
	return m
}

func metaContent(raw, key string) string {
	i := strings.Index(raw, key)
	if i < 0 {
		return ""
	}
	j := strings.Index(raw[i:], "content=\"")
	if j < 0 {
		return ""
	}
	rest := raw[i+j+9:]
	if k := strings.Index(rest, "\""); k >= 0 {
		return rest[:k]
	}
	return ""
}

// --- 10 ----------------------------------------------------------------------

func selfCustody() {
	user, _, _ := evm.GenerateKey() // stands in for the browser wallet of a person
	addr := user.Address()
	signWith := func(k *evm.PrivateKey, td string) string {
		p, err := evm.ParseTypedData(td)
		if err != nil {
			return ""
		}
		d, _ := p.Digest()
		sig, _ := k.Sign(d)
		return evm.SigHex(sig)
	}
	code, q := post("/v1/wallet/quote", M{"address": addr, "sizeUsd": 20, "takeProfitPct": 2, "stopLossPct": 1})
	x, _ := q["quote"].(M)
	if !check("quote for your own wallet", code == 200 && x != nil && s(x, "entryTypedData") != "", fmt.Sprintf("HTTP %d %s", code, errMsg(q))) {
		return
	}
	check("payload is a FlashOrder for your address", strings.Contains(strings.ToLower(s(x, "entryTypedData")), strings.ToLower(addr[2:])), "")
	other, _, _ := evm.GenerateKey()
	c, r := post("/v1/wallet/order", M{"quoteId": s(x, "quoteId"), "address": addr, "entrySignature": signWith(other, s(x, "entryTypedData")), "bracketSignature": signWith(other, s(x, "bracketTypedData"))})
	check("signature from another wallet refused", c != 201, fmt.Sprintf("HTTP %d %s", c, firstNonEmptyS(s(r, "error"), errMsg(r))))
	_, q = post("/v1/wallet/quote", M{"address": addr, "sizeUsd": 20, "takeProfitPct": 2, "stopLossPct": 1})
	x, _ = q["quote"].(M)
	c, r = post("/v1/wallet/order", M{"quoteId": s(x, "quoteId"), "address": addr, "entrySignature": signWith(user, s(x, "entryTypedData")), "bracketSignature": signWith(user, s(x, "bracketTypedData"))})
	tr, _ := r["trade"].(M)
	if check("order signed in your wallet placed", c == 201 && tr != nil && s(tr, "kind") == "self", fmt.Sprintf("HTTP %d %s", c, firstNonEmptyS(s(r, "error"), errMsg(r)))) {
		t := waitTrade(s(tr, "id"), filledActive)
		check("your order filled, bracket active", filledActive(t), s(t, "status")+" / "+s(t, "protection"))
	}
	c, r = post("/v1/admin/trades/"+s(tr, "id")+"/exit", nil)
	check("the arena cannot exit your position for you", c != 200, firstNonEmptyS(errMsg(r), fmt.Sprint("HTTP ", c)))
	if sg := list(get("/v1/signals"), "signals"); len(sg) > 0 {
		copier, _, _ := evm.GenerateKey()
		src := s(firstM(sg), "id")
		_, q = post("/v1/wallet/quote", M{"address": copier.Address(), "sizeUsd": 20, "copyOf": src})
		x, _ = q["quote"].(M)
		if x != nil {
			c, r = post("/v1/wallet/order", M{"quoteId": s(x, "quoteId"), "address": copier.Address(), "entrySignature": signWith(copier, s(x, "entryTypedData")), "bracketSignature": signWith(copier, s(x, "bracketTypedData"))})
		}
		check("copy a signal from your own wallet", x != nil && c == 201 && s(r, "trade", "copyOf") == src, fmt.Sprintf("HTTP %d %s %s", c, errMsg(q), firstNonEmptyS(s(r, "error"), errMsg(r))))
	}
	me := get("/v1/wallet/" + addr)
	check("your wallet page lists your trades", len(list(me, "trades")) >= 1, fmt.Sprintf("%d trades, USDC %.2f", len(list(me, "trades")), f(me, "usdc")))
	ch := get("/v1/chains")
	check("chains Flash executes on are listed", s(ch, "home") == "base" && len(list(ch, "chains")) >= 1 && len(list(ch, "tradeChains")) >= 1,
		fmt.Sprintf("%d chains, %d tradeable", len(list(ch, "chains")), len(list(ch, "tradeChains"))))
	c, r = post("/v1/wallet/quote", M{"address": addr, "chain": "arbitrum", "sizeUsd": 20, "takeProfitPct": 2, "stopLossPct": 1})
	check("paper money stays on Base (another chain is real money)", c == 422 && s(r, "decision", "code") == "CHAIN", fmt.Sprintf("HTTP %d %s", c, errMsg(r)))
}

func firstNonEmptyS(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- 11 ----------------------------------------------------------------------

// meDo calls a /v1/me endpoint with a wallet session (no admin token).
func meDo(method, p string, body any, session string) (int, M) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, *base+p, rd)
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Arena-Session", session)
	}
	res, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return 0, M{"error": err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m M
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = M{}
	}
	return res.StatusCode, m
}

func signIn(key *evm.PrivateKey) (string, M) {
	_, n := meDo("POST", "/v1/me/nonce", M{"address": key.Address()}, "")
	sig, _ := key.Sign(evm.PersonalMessageHash([]byte(s(n, "message"))))
	body := M{"address": key.Address(), "signature": evm.SigHex(sig), "nonce": s(n, "nonce")}
	_, l := meDo("POST", "/v1/me/login", body, "")
	return s(l, "token"), body
}

func signTyped(key *evm.PrivateKey, raw string) string {
	td, err := evm.ParseTypedData(raw)
	if err != nil {
		return ""
	}
	d, _ := td.Digest()
	sig, _ := key.Sign(d)
	return evm.SigHex(sig)
}

func gaslessAccount() {
	user, _, _ := evm.GenerateKey()
	stranger, _, _ := evm.GenerateKey()
	tok, used := signIn(user)
	check("wallet sign-in (free message)", strings.HasPrefix(tok, "u_"), user.Address()[:12]+"…")
	c, _ := meDo("POST", "/v1/me/login", used, "")
	check("a sign-in message works once", c == 401, fmt.Sprint("replay: HTTP ", c))
	c, _ = meDo("GET", "/v1/me", nil, "")
	check("account calls need the session", c == 401, fmt.Sprint("HTTP ", c))

	c, me := meDo("POST", "/v1/me/account", M{}, tok)
	acct := s(me, "account", "address")
	check("account created, payout locked to the signer", c == 201 && acct != "" && strings.EqualFold(s(me, "account", "profile", "payout"), user.Address()),
		fmt.Sprintf("HTTP %d, account %s, payout %s", c, short(acct), short(s(me, "account", "profile", "payout"))))
	c, again := meDo("POST", "/v1/me/account", M{}, tok)
	check("same address, same account", c == 200 && s(again, "account", "address") == acct, fmt.Sprint("HTTP ", c))
	before := f(me, "account", "usdc")

	c, p := meDo("POST", "/v1/me/deposit/prepare", M{"amountUsd": 12}, tok)
	check("deposit = a USDC EIP-3009 authorization", c == 200 && strings.Contains(s(p, "typedData"), "TransferWithAuthorization") &&
		strings.EqualFold(s(p, "to"), acct), fmt.Sprintf("$%.2f to %s", f(p, "amountUsd"), short(s(p, "to"))))
	c, bad := meDo("POST", "/v1/me/deposit", M{"nonce": s(p, "nonce"), "signature": signTyped(stranger, s(p, "typedData"))}, tok)
	check("someone else's signature is refused", c == 400, errMsg(bad))

	_, p = meDo("POST", "/v1/me/deposit/prepare", M{"amountUsd": 12}, tok)
	good := M{"nonce": s(p, "nonce"), "signature": signTyped(user, s(p, "typedData"))}
	c, d := meDo("POST", "/v1/me/deposit", good, tok)
	after := f(d, "account", "usdc")
	check("signed deposit lands, no gas from the user", c == 200 && after-before > 11.99 && after-before < 12.01,
		fmt.Sprintf("tx %s, USDC %.2f -> %.2f", s(d, "tx"), before, after))
	c, _ = meDo("POST", "/v1/me/deposit", good, tok)
	check("the same signature cannot be used twice", c == 410, fmt.Sprint("HTTP ", c))

	c, iv := meDo("POST", "/v1/me/invest", M{"agentId": "trendhunter", "mode": "invest", "allocationPct": 25, "maxPerTradeUsd": 5, "dailyCapUsd": 15}, tok)
	fl, _ := path(iv, "account", "follows").([]any)
	ok := c == 200 && len(fl) == 1 && s(fl[0], "agentId") == "trendhunter" && s(fl[0], "mode") == "invest"
	check("invest with an agent from the account", ok, fmt.Sprintf("HTTP %d %s", c, errMsg(iv)))

	// Your own auto-trader runs on the same account (no new wallet to fund).
	plan := M{"name": "My auto-trader", "token": "WETH", "strategy": "interval", "intervalMin": 60, "tradeUsd": 5, "budgetUsd": 10,
		"takeProfitPct": 2, "stopLossPct": 1, "durationHours": 1}
	c, _ = meDo("POST", "/v1/me/plans", plan, tok)
	check("one job per account: plan refused while investing", c == 409, fmt.Sprint("HTTP ", c))
	meDo("POST", "/v1/me/stop", M{}, tok)
	// Paused, then four starts at once: one plan is made, and starting it
	// keeps the account's pause and loss limit.
	meDo("POST", "/v1/me/pause", M{}, tok)
	type made struct {
		c int
		m M
	}
	results := make(chan made, 4)
	for i := 0; i < 4; i++ {
		go func() { c, m := meDo("POST", "/v1/me/plans", plan, tok); results <- made{c, m} }()
	}
	var pl M
	n := 0
	for i := 0; i < 4; i++ {
		if r := <-results; r.c == 201 {
			n, pl = n+1, r.m
		}
	}
	check("four plan starts at once make one plan", n == 1, fmt.Sprintf("%d plans made", n))
	_, now := meDo("GET", "/v1/me", nil, tok)
	check("a plan keeps the account's pause and loss limit", path(now, "account", "profile", "paused") == true && f(now, "account", "profile", "lossLimitPct") == 20,
		fmt.Sprintf("paused %v, loss limit %v%%", path(now, "account", "profile", "paused"), f(now, "account", "profile", "lossLimitPct")))
	meDo("POST", "/v1/me/resume", M{}, tok)
	accountID := s(me, "account", "id")
	check("own auto-trader runs on the account", pl != nil && s(pl, "plan", "walletId") == accountID,
		fmt.Sprintf("plan wallet %s %s", s(pl, "plan", "walletId"), errMsg(pl)))
	c, _ = meDo("POST", "/v1/me/invest", M{"agentId": "trendhunter", "mode": "invest"}, tok)
	check("invest refused while the plan runs", c == 409, fmt.Sprint("HTTP ", c))
	stok, _ := signIn(stranger)
	if id := s(pl, "plan", "id"); id != "" {
		c, _ = meDo("POST", "/v1/me/plans/"+id+"/stop", nil, stok)
		check("a stranger cannot stop your plan", c == 403 || c == 409, fmt.Sprint("HTTP ", c))
		c, ps := meDo("POST", "/v1/me/plans/"+id+"/stop", nil, tok)
		check("the owner stops their own plan", c == 200 && s(ps, "plan", "status") == "stopped", fmt.Sprintf("HTTP %d, %s", c, s(ps, "plan", "status")))
	}

	_, other := meDo("GET", "/v1/me", nil, stok)
	check("another wallet cannot see or use it", path(other, "account") == nil, "stranger sees no account")

	c, wd := meDo("POST", "/v1/me/withdraw", M{"token": "USDC"}, tok)
	check("withdraw goes only to the signer's address", c == 200 && strings.EqualFold(s(wd, "to"), user.Address()) && f(wd, "amount") > 0,
		fmt.Sprintf("HTTP %d, %.2f USDC to %s %s", c, f(wd, "amount"), short(s(wd, "to")), errMsg(wd)))
}

// --- 12 ----------------------------------------------------------------------

// account signs a fresh key in, creates its account and deposits usd by signature.
func account(key *evm.PrivateKey, usd float64) (tok, id string) {
	tok, _ = signIn(key)
	_, me := meDo("POST", "/v1/me/account", M{}, tok)
	_, p := meDo("POST", "/v1/me/deposit/prepare", M{"amountUsd": usd}, tok)
	meDo("POST", "/v1/me/deposit", M{"nonce": s(p, "nonce"), "signature": signTyped(key, s(p, "typedData"))}, tok)
	return tok, s(me, "account", "id")
}

func communityAgents() {
	maker, _, _ := evm.GenerateKey()
	fan, _, _ := evm.GenerateKey()
	name := "E2E Kaka " + runTag
	rules := M{"name": name, "tagline": "Buys two red checks", "strategy": "contrarian", "streak": 2, "takeProfitPct": 1.5, "stopLossPct": 1, "sizePct": 25}

	mtok, _ := signIn(maker)
	c, _ := meDo("POST", "/v1/me/agent", rules, mtok)
	check("an agent needs an account first", c == 409, fmt.Sprint("HTTP ", c))
	mtok, id := account(maker, 20)
	c, _ = meDo("POST", "/v1/me/agent", rules, "")
	check("making an agent needs a wallet sign-in", c == 401, fmt.Sprint("HTTP ", c))
	wide := M{}
	for k, v := range rules {
		wide[k] = v
	}
	wide["stopLossPct"] = 9
	c, bad := meDo("POST", "/v1/me/agent", wide, mtok)
	check("a stop loss over 5% is refused", c == 400, errMsg(bad))

	c, ma := meDo("POST", "/v1/me/agent", rules, mtok)
	check("anyone makes an agent with their wallet", c == 201 && s(ma, "agent", "id") == id && s(ma, "agent", "kind") == "contrarian",
		fmt.Sprintf("HTTP %d, %s %s", c, s(ma, "agent", "name"), errMsg(ma)))
	ag := get("/v1/agents/" + id)
	check("public agent page: maker and rules shown", path(ag, "agent", "community") == true && strings.EqualFold(s(ag, "agent", "owner"), maker.Address()) &&
		f(ag, "agent", "rules", "stopLossPct") == 1 && f(ag, "agent", "rules", "takeProfitPct") == 1.5,
		fmt.Sprintf("by %s, TP %.1f%% SL %.1f%%", short(s(ag, "agent", "owner")), f(ag, "agent", "rules", "takeProfitPct"), f(ag, "agent", "rules", "stopLossPct")))
	onBoard := false
	for _, r := range list(get("/v1/leaderboard"), "rows") {
		if s(r, "agentId") == id {
			onBoard = path(r, "community") == true && strings.EqualFold(s(r, "owner"), maker.Address())
		}
	}
	check("listed on the leaderboard as a community agent", onBoard, id)

	ftok, fid := account(fan, 20)
	c, dup := meDo("POST", "/v1/me/agent", rules, ftok)
	check("agent names are unique", c == 409, errMsg(dup))
	c, _ = meDo("POST", "/v1/me/invest", M{"agentId": "trendhunter", "mode": "invest"}, mtok)
	check("an agent account cannot also invest", c == 409, fmt.Sprint("HTTP ", c))
	c, _ = meDo("POST", "/v1/me/plans", planBody("E2E agent plan", "WETH", "interval"), mtok)
	check("an agent account cannot also run a plan", c == 409, fmt.Sprint("HTTP ", c))

	c, iv := meDo("POST", "/v1/me/invest", M{"agentId": id, "mode": "invest", "allocationPct": 25, "maxPerTradeUsd": 5, "dailyCapUsd": 15}, ftok)
	fl := list(iv, "account", "follows")
	check("another person invests with it", c == 200 && len(fl) == 1 && s(fl[0], "agentId") == id, fmt.Sprintf("HTTP %d %s", c, errMsg(iv)))
	c, _ = meDo("POST", "/v1/me/agent/pause", nil, ftok)
	check("only the maker controls the agent", c == 404, fmt.Sprint("HTTP ", c))

	c, pz := meDo("POST", "/v1/me/agent/pause", nil, mtok)
	check("maker pauses the agent", c == 200 && path(pz, "agent", "paused") == true && path(get("/v1/agents/"+id), "agent", "paused") == true, fmt.Sprint("HTTP ", c))
	c, rz := meDo("POST", "/v1/me/agent/resume", nil, mtok)
	check("maker resumes the agent", c == 200 && path(rz, "agent", "paused") == nil, fmt.Sprint("HTTP ", c))

	// It trades on its own: two falling price checks fire the signal. The
	// real pool price may not fall twice in a row for a while, so a quiet
	// market is a SKIP, not a failure.
	var own, inv M
	for end := time.Now().Add(150 * time.Second); time.Now().Before(end) && (own == nil || inv == nil); time.Sleep(5 * time.Second) {
		for _, t := range list(get("/v1/agents/"+id), "trades") {
			tm, _ := t.(M)
			switch {
			case own == nil && s(tm, "kind") == "agent" && s(tm, "walletId") == id:
				own = tm
			case inv == nil && s(tm, "kind") == "invest" && s(tm, "walletId") == fid:
				inv = tm
			}
		}
	}
	if own == nil {
		fmt.Printf("  SKIP  %-44s %s\n", "the agent trades its own signal", "price did not fall 2 checks in a row within 150s")
		return
	}
	// Size is 25% of the account (paper accounts start with paper USDC on top
	// of the $20 deposit), so check the share, not a fixed dollar amount.
	// The public page shows the last equity sample (one a minute), which a
	// brand-new agent may not have yet; the owner's view reads live balances.
	entry, acct := f(own, "entry"), f(get("/v1/agents/"+id), "agent", "equityUsd")
	if _, mine := meDo("GET", "/v1/me", nil, mtok); f(mine, "account", "equityUsd") > acct {
		acct = f(mine, "account", "equityUsd")
	}
	check("the agent trades its own signal with its rules", entry > 0 && math.Abs(f(own, "stopLoss")-entry*0.99) < entry*0.002 &&
		f(own, "sizeUsd") > 0 && f(own, "sizeUsd") <= 0.25*acct*1.05+0.01,
		fmt.Sprintf("$%.2f at %.2f, SL %.2f (%s)", f(own, "sizeUsd"), entry, f(own, "stopLoss"), s(own, "status")))
	if inv == nil {
		// The investor's order may be refused by the cost guard when Flash's
		// fixed cost spikes at its small size: that is the guard working.
		for _, p := range list(get("/v1/feed?limit=80"), "posts") {
			if strings.Contains(s(p, "text"), fid) && strings.Contains(s(p, "text"), "cost guard") {
				fmt.Printf("  SKIP  %-44s %s\n", "the investor's account trades the same signal", "its order was refused by the cost guard (Flash cost spike)")
				return
			}
		}
	}
	check("the investor's account trades the same signal", inv != nil && s(inv, "agentId") == id,
		fmt.Sprintf("%s $%.2f (%s)", s(inv, "walletId"), f(inv, "sizeUsd"), s(inv, "status")))
}

func short(a string) string {
	if len(a) > 14 {
		return a[:6] + "…" + a[len(a)-4:]
	}
	return a
}

package httpapi

// Social layer: share pages with preview cards (X, Telegram, WhatsApp,
// Discord and TikTok bios unfurl these), a public signals feed (JSON + RSS)
// that bots and other apps can follow, and the manual order ticket.

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// publicBase is the URL people reached us on (tunnel or proxy aware).
func publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func money(x float64) string {
	sign := ""
	if x < 0 {
		sign, x = "-", -x
	}
	if x >= 1000 {
		return sign + "$" + strconv.FormatFloat(x, 'f', 0, 64)
	}
	return sign + "$" + strconv.FormatFloat(x, 'f', 2, 64)
}

// sharePage renders a tiny page whose meta tags make a rich preview, then
// sends the visitor on to the app route.
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	title, desc, route := "Agent Arena", "AI agents trade on Flash. You choose who to follow.", "#/"
	switch kind {
	case "trade":
		t, ok := s.E.Store.Trade(id)
		if !ok {
			writeErr(w, 404, "trade not found")
			return
		}
		who := s.name(*t)
		title = fmt.Sprintf("%s: LONG %s %s", who, tokenOr(t.Token), money(t.SizeUSD))
		desc = fmt.Sprintf("Entry %s · take profit %s · stop loss %s", money(t.Entry), money(t.TakeProfit), money(t.StopLoss))
		if t.PnLUSD != nil {
			desc = fmt.Sprintf("Closed: %s (%s). %s", money(*t.PnLUSD), t.CloseReason, desc)
		}
		route = "#/trade/" + t.ID
	case "agent":
		p, ok := s.E.Agents[id]
		if !ok {
			writeErr(w, 404, "agent not found")
			return
		}
		_, ret, mdd := leaderboard.Stats(s.E.Store.PerfEquity(id))
		title = fmt.Sprintf("%s on Agent Arena", p.Name)
		desc = fmt.Sprintf("%s Return %.2f%%, worst drop %.2f%%, Arena Score %.2f. Copy it or let it trade for you.", p.Tagline, ret, mdd, leaderboard.Score(ret, mdd))
		route = "#/agent/" + id
	case "plan":
		p, ok := s.E.Store.Plan(id)
		if !ok {
			writeErr(w, 404, "plan not found")
			return
		}
		st, _ := s.E.PlanStats(id)
		title = fmt.Sprintf("%s's auto-trader: %s", p.Owner, p.Name)
		desc = fmt.Sprintf("%s %s with %s, %s a trade, TP %.1f%% / SL %.1f%%. P&L %s over %d trades.", p.Token, styleWord(p.Style), p.Strategy, money(p.TradeUSD), p.TakeProfitPct, p.StopLossPct, money(st.PnLUSD), st.Trades)
		route = "#/plan/" + id
	case "leaderboard":
		title = "Agent Arena leaderboard"
		var rows []string
		for _, row := range s.rankRows() {
			if len(rows) < 3 {
				rows = append(rows, fmt.Sprintf("#%d %s (score %.2f)", row.Rank, row.Name, row.Score))
			}
		}
		desc = strings.Join(rows, " · ")
		route = "#/leaderboard"
	default:
		writeErr(w, 404, "unknown share")
		return
	}
	base := publicBase(r)
	app := base + "/" + route
	esc := html.EscapeString
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title>
<meta name="description" content="%s">
<meta property="og:type" content="website"><meta property="og:site_name" content="Agent Arena">
<meta property="og:title" content="%s"><meta property="og:description" content="%s">
<meta property="og:url" content="%s"><meta property="og:image" content="%s/icons/icon-512.png">
<meta name="twitter:card" content="summary"><meta name="twitter:title" content="%s"><meta name="twitter:description" content="%s">
<meta http-equiv="refresh" content="0; url=%s"></head>
<body><p><a href="%s">%s</a></p></body></html>`,
		esc(title), esc(desc), esc(title), esc(desc), esc(base+r.URL.Path), esc(base), esc(title), esc(desc), esc(app), esc(app), esc(title))
}

func tokenOr(t string) string {
	if t == "" {
		return "WETH"
	}
	return t
}

func styleWord(s string) string {
	switch s {
	case "twap":
		return "DCA"
	case "limit":
		return "limit buys"
	case "stop":
		return "stop buys"
	}
	return "market buys"
}

// name says who made a trade, from its kind. Test and manual orders carry an
// agent id only for bookkeeping, so they must never read "Copy of <agent>".
func (s *Server) name(t store.Trade) string {
	agent := ""
	if p, ok := s.E.Agents[t.AgentID]; ok {
		agent = p.Name
	}
	switch {
	case agent != "" && t.WalletID == t.AgentID:
		return agent
	case t.PlanID != "":
		if p, ok := s.E.Store.Plan(t.PlanID); ok {
			return p.Owner + "'s " + p.Name
		}
		return "Auto-trade plan"
	case t.Kind == "test":
		return "Test trade"
	case t.Kind == "manual":
		return "Manual order"
	case t.Kind == "self":
		return "Own wallet"
	case t.Kind == "invest" && agent != "":
		return "Invested with " + agent
	case agent != "":
		return "Copy of " + agent
	}
	return "Agent Arena"
}

// rankRows is the leaderboard (shared with the share page).
func (s *Server) rankRows() []leaderboard.Row {
	var rows []leaderboard.Row
	for id, p := range s.E.Agents {
		_, ret, mdd := leaderboard.Stats(s.E.Store.PerfEquity(id))
		rows = append(rows, leaderboard.Row{AgentID: id, Name: p.Name, ReturnPct: ret, MaxDrawdown: mdd, Score: leaderboard.Score(ret, mdd)})
	}
	return leaderboard.Rank(rows)
}

type signal struct {
	ID          string   `json:"id"`
	Agent       string   `json:"agent"`
	AgentID     string   `json:"agentId"`
	Token       string   `json:"token"`
	Side        string   `json:"side"`
	Style       string   `json:"style"`
	Entry       float64  `json:"entry"`
	TakeProfit  float64  `json:"takeProfit"`
	StopLoss    float64  `json:"stopLoss"`
	SizeUSD     float64  `json:"sizeUsd"`
	Reason      string   `json:"reason"`
	Status      string   `json:"status"`
	PnLUSD      *float64 `json:"pnlUsd,omitempty"`
	At          string   `json:"at"`
	Share       string   `json:"share"`
	Paper       bool     `json:"paper,omitempty"`
	OrderType   string   `json:"orderType"`
	CloseReason string   `json:"closeReason,omitempty"`
}

// signals are the agents' own trades, newest first: what followers and bots copy.
func (s *Server) signalList(r *http.Request, n int) []signal {
	base := publicBase(r)
	out := []signal{}
	for _, t := range s.E.Store.Trades("", 0) {
		p, ok := s.E.Agents[t.AgentID]
		if !ok || t.WalletID != t.AgentID || t.Test || t.DryRun {
			continue
		}
		out = append(out, signal{ID: t.ID, Agent: p.Name, AgentID: t.AgentID, Token: tokenOr(t.Token), Side: "buy", Style: firstNonEmpty(t.Style, "market"),
			Entry: t.Entry, TakeProfit: t.TakeProfit, StopLoss: t.StopLoss, SizeUSD: t.SizeUSD, Reason: t.Reason, Status: t.Status,
			PnLUSD: t.PnLUSD, At: t.CreatedAt.Format(time.RFC3339), Share: base + "/s/trade/" + t.ID, Paper: t.Paper, OrderType: t.OrderType, CloseReason: t.CloseReason})
		if len(out) >= n {
			break
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Server) signals(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 100 {
		n = 30
	}
	writeJSON(w, 200, map[string]any{"signals": s.signalList(r, n), "rss": publicBase(r) + "/signals.rss"})
}

func (s *Server) signalsRSS(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Title   string `xml:"title"`
		Link    string `xml:"link"`
		GUID    string `xml:"guid"`
		PubDate string `xml:"pubDate"`
		Desc    string `xml:"description"`
	}
	type channel struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		Desc  string `xml:"description"`
		Items []item `xml:"item"`
	}
	ch := channel{Title: "Agent Arena signals", Link: publicBase(r), Desc: "Every trade the arena's AI agents place on Flash"}
	for _, sg := range s.signalList(r, 30) {
		at, _ := time.Parse(time.RFC3339, sg.At)
		title := fmt.Sprintf("%s: %s %s %s", sg.Agent, strings.ToUpper(sg.Side), sg.Token, money(sg.SizeUSD))
		if sg.PnLUSD != nil {
			title += " · closed " + money(*sg.PnLUSD)
		}
		ch.Items = append(ch.Items, item{Title: title, Link: sg.Share, GUID: sg.ID, PubDate: at.Format(time.RFC1123Z),
			Desc: fmt.Sprintf("Entry %s, TP %s, SL %s. %s", money(sg.Entry), money(sg.TakeProfit), money(sg.StopLoss), sg.Reason)})
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel channel  `xml:"channel"`
	}{Version: "2.0", Channel: ch})
}

// orderTicket places one order of any Flash style from a sub-wallet. The
// size is the operator's choice; the referee and every guard still apply.
func (s *Server) orderTicket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WalletID       string  `json:"walletId"`
		Token          string  `json:"token"`
		Style          string  `json:"style"`
		SizeUSD        float64 `json:"sizeUsd"`
		TakeProfitPct  float64 `json:"takeProfitPct"`
		StopLossPct    float64 `json:"stopLossPct"`
		LimitPct       float64 `json:"limitPct"`
		StopPct        float64 `json:"stopPct"`
		TwapMinutes    int     `json:"twapMinutes"`
		Protect        string  `json:"protect"`
		TrailPct       float64 `json:"trailPct"`
		MaxSlippagePct float64 `json:"maxSlippagePct"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	wl, ok := s.E.Wallets.Get(in.WalletID)
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	tok, ok := s.E.Cfg.TokenBySymbol(firstNonEmpty(in.Token, s.E.Cfg.TradeAsset))
	if !ok {
		writeErr(w, 400, "unknown coin")
		return
	}
	px := s.E.TokenPrice(tok.Address)
	if px <= 0 {
		writeErr(w, 503, "no live price for "+tok.Symbol+" yet")
		return
	}
	if in.TakeProfitPct == 0 {
		in.TakeProfitPct = 2
	}
	if in.StopLossPct == 0 {
		in.StopLossPct = 1
	}
	if in.SizeUSD < 1 {
		writeErr(w, 400, "size must be at least $1")
		return
	}
	in.Style = firstNonEmpty(in.Style, "market")
	intent := referee.Intent{AgentID: agentFor(s, wl), WalletID: wl.ID, Token: tok.Symbol, SizeUSD: in.SizeUSD, OwnSize: true, Price: px,
		Style: in.Style, TwapMinutes: in.TwapMinutes, ProtectMode: in.Protect, TrailPct: in.TrailPct,
		Reason: "manual " + engine.StyleName(in.Style) + " order"}
	if in.MaxSlippagePct > 0 {
		intent.MaxSlippage = strconv.FormatFloat(in.MaxSlippagePct/100, 'f', -1, 64)
	}
	entry := px
	switch in.Style {
	case "limit":
		intent.EntryLimit = round2(px * (1 - in.LimitPct/100))
		entry = intent.EntryLimit
	case "stop":
		intent.StopPrice = round2(px * (1 + in.StopPct/100))
		entry = intent.StopPrice
	}
	intent.TakeProfit, intent.StopLoss = round2(entry*(1+in.TakeProfitPct/100)), round2(entry*(1-in.StopLossPct/100))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res := s.E.Propose(ctx, intent)
	code := 200
	if !res.Decision.OK {
		code = 422
	}
	if res.Error != "" {
		code = 502
	}
	writeJSON(w, code, res)
}

// agentFor labels a manual order: the agent itself, or the agent a
// follower wallet copies / invests with.
func agentFor(s *Server, wl *wallet.Wallet) string {
	if _, ok := s.E.Agents[wl.ID]; ok {
		return wl.ID
	}
	for _, f := range s.E.Store.FollowsOf(wl.ID) {
		return f.AgentID
	}
	return ""
}

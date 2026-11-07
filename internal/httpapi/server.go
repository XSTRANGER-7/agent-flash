// Package httpapi is the public + admin REST API (stdlib net/http, Go 1.22
// routing). Public reads are open; anything that moves money or changes
// state is behind the admin bearer token (or a verified World proof).
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/approval"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/outbox"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
	"github.com/vikram/agent-arena/internal/web"
)

type Server struct {
	E     *engine.Engine
	Log   *slog.Logger
	start time.Time
}

func New(e *engine.Engine, log *slog.Logger) *Server {
	s := &Server{E: e, Log: log, start: time.Now()}
	if e != nil && e.Cfg != nil {
		s.loadSessions() // wallet sign-ins survive restarts
	}
	return s
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	// health + ops
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	m.HandleFunc("GET /readyz", s.ready)
	m.Handle("GET /metrics", metrics.Handler())
	// public reads
	m.HandleFunc("GET /v1/agents", s.agents)
	m.HandleFunc("GET /v1/agents/{id}", s.agent)
	m.HandleFunc("GET /v1/leaderboard", s.leaderboard)
	m.HandleFunc("GET /v1/trades", s.trades)
	m.HandleFunc("GET /v1/trades/{id}", s.trade)
	m.HandleFunc("GET /v1/trades/{id}/chart", s.tradeChart)
	m.HandleFunc("POST /v1/backtest", s.backtest)
	m.HandleFunc("GET /v1/feed", s.feedList)
	m.HandleFunc("GET /v1/feed/stream", s.feedStream)
	m.HandleFunc("GET /v1/price", s.price)
	m.HandleFunc("GET /v1/flash/status", s.flashStatus)
	m.HandleFunc("GET /v1/stats", s.stats)
	m.HandleFunc("GET /v1/proof", s.proof)
	// "My account": sign in with a wallet, gasless deposits, invest, withdraw (me.go)
	m.HandleFunc("POST /v1/me/nonce", s.meNonce)
	m.HandleFunc("POST /v1/me/login", s.meLogin)
	m.HandleFunc("GET /v1/me", s.me(s.meGet))
	m.HandleFunc("POST /v1/me/account", s.me(s.meAccount))
	m.HandleFunc("POST /v1/me/logout", s.me(s.meLogout))
	m.HandleFunc("POST /v1/me/deposit/prepare", s.me(s.meDepositPrepare))
	m.HandleFunc("POST /v1/me/deposit", s.me(s.meDeposit))
	m.HandleFunc("POST /v1/me/invest", s.me(s.meInvest))
	m.HandleFunc("POST /v1/me/stop", s.me(s.meStop))
	m.HandleFunc("POST /v1/me/pause", s.me(s.mePause))
	m.HandleFunc("POST /v1/me/resume", s.me(s.mePause))
	m.HandleFunc("POST /v1/me/withdraw", s.me(s.meWithdraw))
	m.HandleFunc("POST /v1/me/plans", s.me(s.mePlan))
	m.HandleFunc("POST /v1/me/plans/{id}/{action}", s.me(s.mePlanAction))
	m.HandleFunc("POST /v1/me/agent", s.me(s.meAgent))
	m.HandleFunc("POST /v1/me/agent/{action}", s.me(s.meAgentAction))
	m.HandleFunc("POST /v1/me/wallets/{id}/{action}", s.me(s.meSubWallet))
	m.HandleFunc("GET /v1/gas", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.E.Exec.GasTankStatus(r.Context())) })
	m.HandleFunc("GET /v1/approvals/{id}", s.approvalGet)
	m.HandleFunc("POST /v1/predictions", s.predict)
	// World Agent Pass callback (proof verified server-side)
	m.HandleFunc("POST /v1/approvals/{id}/world", s.approvalWorld)
	// admin (bearer token)
	m.HandleFunc("GET /v1/admin/approvals", s.admin(s.approvalsList))
	m.HandleFunc("POST /v1/admin/approvals/{id}/{decision}", s.admin(s.approvalDecide))
	m.HandleFunc("POST /v1/admin/follows", s.admin(s.follow))
	m.HandleFunc("DELETE /v1/admin/follows/{follower}/{agent}", s.admin(s.unfollow))
	m.HandleFunc("POST /v1/admin/propose", s.admin(s.propose))
	m.HandleFunc("POST /v1/admin/trades/{id}/cancel", s.admin(s.cancel))
	m.HandleFunc("POST /v1/admin/trades/{id}/exit", s.admin(s.exit))
	m.HandleFunc("POST /v1/admin/trades/{id}/bracket", s.admin(s.rebracket))
	m.HandleFunc("GET /v1/admin/wallets", s.admin(s.walletsList))
	m.HandleFunc("POST /v1/admin/wallets", s.admin(s.walletCreate))
	m.HandleFunc("GET /v1/admin/wallets/{id}", s.admin(s.walletGet))
	m.HandleFunc("PATCH /v1/admin/wallets/{id}", s.admin(s.walletLimits))
	m.HandleFunc("POST /v1/admin/wallets/{id}/withdraw", s.admin(s.walletWithdraw))
	m.HandleFunc("POST /v1/admin/wallets/{id}/revoke", s.admin(s.walletRevoke))
	m.HandleFunc("POST /v1/admin/wallets/{id}/topup", s.admin(s.paperTopUp))
	m.HandleFunc("GET /v1/admin/wallets/{id}/orders", s.admin(s.walletOrders))
	m.HandleFunc("POST /v1/admin/wallets/{id}/{action}", s.admin(s.walletPause))
	m.HandleFunc("POST /v1/admin/test-trade", s.admin(s.testTrade))
	m.HandleFunc("GET /v1/tokens", s.tokens)
	m.HandleFunc("GET /v1/signals", s.signals)
	m.HandleFunc("POST /v1/wallet/quote", s.selfQuote)
	m.HandleFunc("POST /v1/wallet/order", s.selfOrder)
	m.HandleFunc("GET /v1/wallet/{address}", s.selfWallet)
	m.HandleFunc("GET /signals.rss", s.signalsRSS)
	m.HandleFunc("GET /s/{kind}", s.sharePage)
	m.HandleFunc("GET /s/{kind}/{id}", s.sharePage)
	m.HandleFunc("POST /v1/admin/orders", s.admin(s.orderTicket))
	m.HandleFunc("GET /v1/plans", s.plansList)
	m.HandleFunc("GET /v1/plans/{id}", s.planGet)
	m.HandleFunc("POST /v1/admin/plans", s.admin(s.planCreate))
	m.HandleFunc("POST /v1/admin/plans/{id}/{action}", s.admin(s.planAction))
	m.HandleFunc("POST /v1/admin/emergency-stop", s.admin(s.emergencyStop))
	m.HandleFunc("POST /v1/admin/resume", s.admin(s.resumeAll))
	m.HandleFunc("GET /v1/admin/follows", s.admin(s.follows))
	// the PWA (same origin, CSP-protected)
	m.Handle("GET /", web.Handler())
	return s.middleware(m)
}

// ---- middleware ------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// hasGetRoute reports whether the path has a GET route (so another method
// on it is "not allowed" rather than "not found").
func hasGetRoute(mux *http.ServeMux, r *http.Request) bool {
	g := r.Clone(r.Context())
	g.Method = http.MethodGet
	_, p := mux.Handler(g)
	return p != "" && p != "GET /"
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	origins := map[string]bool{}
	for _, o := range s.E.Cfg.CORSOrigins {
		origins[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic", "path", r.URL.Path, "err", rec)
				writeErr(w, 500, "internal error")
			}
		}()
		if o := r.Header.Get("Origin"); o != "" && (origins[o] || origins["*"]) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Arena-Session")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/v1/me") || strings.HasPrefix(r.URL.Path, "/v1/admin") {
			w.Header().Set("Cache-Control", "no-store") // personal and operator data
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		sw := &statusWriter{ResponseWriter: w, code: 200}
		if mux, ok := next.(*http.ServeMux); ok && strings.HasPrefix(r.URL.Path, "/v1/") {
			if _, pattern := mux.Handler(r); pattern == "" {
				// No API route: answer in JSON like every other API error
				// (the router's own reply is plain text).
				code := 404
				if h := w.Header(); r.Method != http.MethodGet && hasGetRoute(mux, r) {
					code = 405
					h.Set("Allow", "GET")
				}
				writeErr(sw, code, http.StatusText(code))
				s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", code, "ms", time.Since(start).Milliseconds())
				return
			}
		}
		next.ServeHTTP(sw, r)
		metrics.Inc("http_requests_total")
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.E.Cfg.AdminToken)) != 1 {
			writeErr(w, 401, "unauthorized")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// ---- handlers --------------------------------------------------------------

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	_, fresh := s.E.Prices.Snapshot(3 * s.E.Cfg.PriceEvery)
	code := 200
	if !fresh {
		code = 503
	}
	writeJSON(w, code, map[string]any{"priceFresh": fresh, "dryRun": s.E.Cfg.DryRun, "mode": s.E.Cfg.ModeName(), "outboxPending": s.E.Outbox.Pending(), "uptimeSec": int(time.Since(s.start).Seconds())})
}

type agentView struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Tagline   string  `json:"tagline"`
	Strategy  string  `json:"strategy"`
	Address   string  `json:"address"`
	Followers int     `json:"followers"`
	EquityUSD float64 `json:"equityUsd"`
	Community bool    `json:"community,omitempty"`
	Owner     string  `json:"owner,omitempty"`
	// Rules of an agent a person made (TP / SL / size, and the strategy settings).
	Rules  *store.UserAgent `json:"rules,omitempty"`
	Paused bool             `json:"paused,omitempty"`
}

func (s *Server) agentViews() []agentView {
	var out []agentView
	for id, p := range s.E.AgentMap() {
		w, ok := s.E.Wallets.Get(id)
		if !ok {
			continue
		}
		eq, _, _ := leaderboard.Stats(s.E.Store.Equity(id))
		v := agentView{ID: id, Name: p.Name, Tagline: p.Tagline, Strategy: p.Strategy, Address: w.Address(), Followers: s.E.Store.FollowerCount(id), EquityUSD: eq,
			Community: p.Community, Owner: p.Owner}
		if ua, ok := s.E.Store.UserAgent(id); ok {
			v.Rules, v.Paused = &ua, ua.Paused
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"agents": s.agentViews()})
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, a := range s.agentViews() {
		if a.ID == id {
			writeJSON(w, 200, map[string]any{"agent": a, "trades": s.E.Store.Trades(id, 50), "equity": s.E.Store.Equity(id)})
			return
		}
	}
	writeErr(w, 404, "agent not found")
}

func (s *Server) leaderboard(w http.ResponseWriter, r *http.Request) {
	var rows []leaderboard.Row
	for id, p := range s.E.AgentMap() {
		eq, _, _ := leaderboard.Stats(s.E.Store.Equity(id))
		_, ret, mdd := leaderboard.Stats(s.E.Store.PerfEquity(id)) // deposits are not returns
		trades := s.E.Store.Trades(id, 0)
		n, closed, wins := 0, 0, 0
		for _, t := range trades {
			if t.CopyOf != "" || t.WalletID != id || t.Test {
				continue // copies, investor wallets and test trades are not the agent's record
			}
			n++
			if t.PnLUSD != nil {
				closed++
				if *t.PnLUSD > 0 {
					wins++
				}
			}
		}
		wr := 0.0 // over finished trades only, so open positions don't drag it down
		if closed > 0 {
			wr = float64(wins) / float64(closed) * 100
		}
		rows = append(rows, leaderboard.Row{AgentID: id, Name: p.Name, EquityUSD: eq, ReturnPct: ret, MaxDrawdown: mdd, Score: leaderboard.Score(ret, mdd), Trades: n, Closed: closed, Wins: wins, WinRatePct: wr, Followers: s.E.Store.FollowerCount(id),
			Strategy: p.Strategy, Community: p.Community, Owner: p.Owner})
	}
	writeJSON(w, 200, map[string]any{"season": s.E.Cfg.SeasonID, "formula": "Return% - 0.5 * MaxDrawdown%", "rows": leaderboard.Rank(rows)})
}

func (s *Server) trades(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	writeJSON(w, 200, map[string]any{"trades": s.E.Store.Trades(r.URL.Query().Get("agent"), limit)})
}

func (s *Server) trade(w http.ResponseWriter, r *http.Request) {
	t, ok := s.E.Store.Trade(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "trade not found")
		return
	}
	win, lose := s.E.Store.PredictionTally(t.ID)
	writeJSON(w, 200, map[string]any{"trade": t, "predictions": map[string]int{"win": win, "lose": lose}})
}

// feedList pages through the saved feed, newest first: before = the id of
// the last post you have, filter = all | trades | copies | blocked |
// approvals, q = words in the text, agent = an agent id.
func (s *Server) feedList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("limit"))
	if n <= 0 || n > 200 {
		n = 50
	}
	filter, agent := q.Get("filter"), q.Get("agent")
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	if len(text) > 80 {
		text = text[:80]
	}
	keep := func(p feed.Post) bool {
		return feedKindMatch(filter, p.Kind) && (agent == "" || p.AgentID == agent) &&
			(text == "" || strings.Contains(strings.ToLower(p.Text), text))
	}
	posts, more, total := s.E.Feed.Page(q.Get("before"), n, keep)
	out := map[string]any{"posts": posts, "more": more, "total": total}
	if t := s.E.Feed.Oldest(); !t.IsZero() {
		out["since"] = t
	}
	writeJSON(w, 200, out)
}

// feedKindMatch mirrors the feed page's filter chips.
func feedKindMatch(filter, kind string) bool {
	switch filter {
	case "trades":
		return kind == "trade_placed" || kind == "trade_filled" || kind == "trade_closed"
	case "copies":
		return kind == "copy_placed" || kind == "follow"
	case "blocked":
		return strings.HasSuffix(kind, "blocked")
	case "approvals":
		return kind == "approval"
	}
	return true
}

// feedStream is Server-Sent Events: live feed for the frontend.
func (s *Server) feedStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch, unsub := s.E.Feed.Subscribe()
	defer unsub()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case p := <-ch:
			b, _ := json.Marshal(p)
			fmt.Fprintf(w, "event: post\ndata: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) price(w http.ResponseWriter, r *http.Request) {
	hist, fresh := s.E.Prices.Snapshot(3 * s.E.Cfg.PriceEvery)
	if len(hist) > 100 {
		hist = hist[len(hist)-100:]
	}
	st := s.E.Prices.Status()
	writeJSON(w, 200, map[string]any{"symbol": s.E.Cfg.TradeAsset, "last": s.E.Prices.Last(), "fresh": fresh, "history": hist,
		"source": st.Source, "pool": st.Pool, "liquidityUsd": st.LiquidityUSD, "liquiditySource": st.LiquiditySource})
}

// flashStatus is the public view of the Flash connection (no secrets).
func (s *Server) flashStatus(w http.ResponseWriter, r *http.Request) {
	halted, haltWhy := s.E.Halted()
	st := s.E.Prices.Status()
	stream := s.E.StreamState()
	if stream != "live" {
		stream = "polling"
		if !s.E.Cfg.StreamEnabled {
			stream = "off"
		}
	}
	writeJSON(w, 200, map[string]any{
		"dryRun": s.E.Cfg.DryRun, "mode": s.E.Cfg.ModeName(), "liveAgents": s.E.Cfg.LiveAgents,
		"liveBudgetUsd": s.E.Cfg.LiveBudgetUSD, "liveSpentUsd": s.E.LiveSpent(), "halted": halted, "haltReason": haltWhy,
		"testTradeUsd": s.E.Cfg.TestTradeUSD, "testMaxCostPct": s.E.Cfg.TestMaxCostPct, "paperUsdc": s.E.Cfg.PaperUSDC,
		"chainId": s.E.Cfg.ChainID, "stream": stream, "liveQuote": s.E.LiveQuote(),
		"searchEligible": st.SearchEligible, "priceSource": st.Source, "pool": st.Pool, "poolError": st.PoolError,
		"liquidityUsd": st.LiquidityUSD, "liquiditySource": st.LiquiditySource, "riskChecked": st.RiskChecked,
		"settlement": s.E.Exec.Settlement(), "lastPriceAt": st.LastPriceAt,
		"feeBps": s.E.Cfg.FeeBps, "maxSlippage": s.E.Cfg.MaxSlippage, "maxEntryCostPct": s.E.Cfg.MaxEntryCostPct,
		"netTargets": s.E.Cfg.NetTargets, "breakEvenStop": s.E.Cfg.BreakEvenStop,
	})
}

func (s *Server) predict(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TradeID string `json:"tradeId"`
		Voter   string `json:"voter"` // wallet address or user id from your auth layer
		Win     bool   `json:"win"`
	}
	if err := decode(r, &in); err != nil || in.TradeID == "" || in.Voter == "" || len(in.Voter) > 64 {
		writeErr(w, 400, "tradeId and voter are required")
		return
	}
	// Voter ids are anonymous, so the limit is per client: a burst of made-up
	// voters from one place cannot stuff a tally.
	if !allow("vote", r, 30, time.Hour) {
		writeErr(w, 429, "too many votes from here; try later")
		return
	}
	p := &store.Prediction{TradeID: in.TradeID, Voter: strings.ToLower(in.Voter), Win: in.Win, CreatedAt: time.Now().UTC()}
	if err := s.E.Store.Predict(p); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.E.Outbox.Publish(outbox.Event{ID: "prediction:" + in.TradeID + ":" + p.Voter, Kind: "prediction", TTLHours: 48,
		Attributes: map[string]any{"type": "prediction", "season": s.E.Cfg.SeasonID, "tradeid": in.TradeID, "win": boolNum(in.Win)}, Payload: p})
	win, lose := s.E.Store.PredictionTally(in.TradeID)
	writeJSON(w, 201, map[string]any{"ok": true, "win": win, "lose": lose})
}

func boolNum(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) approvalGet(w http.ResponseWriter, r *http.Request) {
	if a := s.E.Approvals.Get(r.PathValue("id")); a != nil {
		writeJSON(w, 200, a)
		return
	}
	writeErr(w, 404, "approval not found")
}

func (s *Server) approvalWorld(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Approve bool            `json:"approve"`
		Proof   json.RawMessage `json:"proof"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad body")
		return
	}
	a, err := s.E.Approvals.WorldDecide(r.Context(), r.PathValue("id"), in.Approve, in.Proof)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) approvalsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"approvals": s.E.Approvals.List(r.URL.Query().Get("all") != "1")})
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request) {
	d := r.PathValue("decision")
	if d != "approve" && d != "reject" {
		writeErr(w, 400, "decision must be approve or reject")
		return
	}
	a, err := s.E.Approvals.AdminDecide(r.PathValue("id"), d == "approve")
	if err != nil {
		code := 400
		if errors.Is(err, approval.ErrNotPending) {
			code = 409
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) follow(w http.ResponseWriter, r *http.Request) {
	var f store.Follow
	if err := decode(r, &f); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	fw, ok := s.E.Wallets.Get(f.FollowerID)
	if !ok || fw.Role != wallet.RoleFollower {
		writeErr(w, 400, "followerId must be a follower wallet in wallets.json")
		return
	}
	if _, ok := s.E.AgentMap()[f.AgentID]; !ok {
		writeErr(w, 400, "unknown agent")
		return
	}
	if f.Mode == "" {
		f.Mode = "copy"
	}
	if f.Mode != "copy" && f.Mode != "invest" {
		writeErr(w, 400, "mode must be copy or invest")
		return
	}
	nf, err := s.followFrom(fw.ID, f.AgentID, f.Mode, f.AllocationPct, f.MaxPerTradeUSD, f.DailyCapUSD)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f = *nf
	s.E.Store.PutFollow(&f)
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: f.AgentID, Text: fmt.Sprintf("%s is now copying %s (max $%.2f per trade)", f.FollowerID, f.AgentID, f.MaxPerTradeUSD)})
	writeJSON(w, 201, f)
}

func (s *Server) unfollow(w http.ResponseWriter, r *http.Request) {
	if !s.E.Store.Unfollow(r.PathValue("follower"), r.PathValue("agent")) {
		writeErr(w, 404, "follow not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// propose lets an operator inject a trade for a demo. It still goes through
// the referee, approvals and executor; there is no bypass.
func (s *Server) propose(w http.ResponseWriter, r *http.Request) {
	var in referee.Intent
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Token == "" {
		in.Token = s.E.Cfg.TradeAsset
	}
	if in.Price == 0 {
		in.Price = s.E.Prices.Last()
	}
	if in.OrderType == "" {
		in.OrderType = "market+bracket"
	}
	if in.WalletID == "" {
		in.WalletID = in.AgentID
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res := s.E.Propose(ctx, in)
	if _, isAgent := s.E.AgentMap()[in.WalletID]; isAgent && in.WalletID == in.AgentID {
		// An operator-injected agent decision also trades the agent's investors.
		go s.E.InvestAll(context.WithoutCancel(r.Context()), in)
	}
	code := 200
	if !res.Decision.OK {
		code = 422
	}
	if res.Error != "" {
		code = 502
	}
	writeJSON(w, code, res)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	t, ok := s.E.Store.Trade(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "trade not found")
		return
	}
	if t.Status != "placed" {
		writeErr(w, 409, "only an unfilled entry can be cancelled; use exit for a filled position")
		return
	}
	wal, ok := s.E.Wallets.Get(t.WalletID)
	if !ok {
		writeErr(w, 400, "wallet missing")
		return
	}
	if err := s.E.Exec.Cancel(r.Context(), wal, t.OrderID); err != nil {
		var ae *flash.APIError
		if errors.As(err, &ae) && ae.HTTPStatus == 422 {
			s.E.RefreshTrade(r.Context(), t.ID) // it filled in the meantime
			writeErr(w, 409, "the entry already filled; use exit to close the position")
			return
		}
		writeErr(w, 502, err.Error())
		return
	}
	// Flash confirms the cancel; read the order back so a partial fill is not lost.
	s.E.RefreshTrade(r.Context(), t.ID)
	if cur, ok := s.E.Store.Trade(t.ID); ok && cur.Status == "placed" {
		s.E.Store.UpdateTrade(t.ID, func(x *store.Trade) {
			x.Status, x.CloseReason = "cancelled", "cancelled by the operator"
			x.Timeline = append(x.Timeline, store.Step{At: time.Now().UTC(), Step: "closed", Detail: "entry cancelled by the operator"})
		})
		s.E.Referee.Closed(t.WalletID)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// exit cancels the bracket and sells the position at market.
func (s *Server) exit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	t, err := s.E.ExitPosition(ctx, r.PathValue("id"))
	if err != nil {
		code := 502
		if errors.Is(err, engine.ErrNotExitable) {
			code = 409
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "orderId": t.ExitOrderID, "trade": t})
}

// rebracket moves the take profit and stop loss of a live trade.
func (s *Server) rebracket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TakeProfit float64 `json:"takeProfit"`
		StopLoss   float64 `json:"stopLoss"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	t, err := s.E.Rebracket(ctx, r.PathValue("id"), in.TakeProfit, in.StopLoss)
	if err != nil {
		code := 400
		switch {
		case errors.Is(err, engine.ErrNotExitable):
			code = 409
		case errors.Is(err, engine.ErrTradeNotFound):
			code = 404
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "trade": t})
}

// stats is the arena summary for the home screen.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	all := s.E.Store.Trades("", 0)
	vol, open, copies, routed := 0.0, 0, 0, 0.0
	for _, t := range all {
		if t.DryRun {
			continue
		}
		vol += t.SizeUSD
		routed += t.USDCSpent + t.USDCReceived // both legs pay the integrator fee
		if t.Open() {
			open++
		}
		if t.CopyOf != "" {
			copies++
		}
	}
	followers := 0
	for id := range s.E.AgentMap() {
		followers += s.E.Store.FollowerCount(id)
	}
	_, fresh := s.E.Prices.Snapshot(3 * s.E.Cfg.PriceEvery)
	feeBps, _ := strconv.ParseFloat(s.E.Cfg.FeeBps, 64)
	writeJSON(w, 200, map[string]any{
		"season": s.E.Cfg.SeasonID, "agents": len(s.E.AgentMap()), "trades": len(all), "openTrades": open,
		"copies": copies, "volumeUsd": vol, "followers": followers, "dryRun": s.E.Cfg.DryRun, "mode": s.E.Cfg.ModeName(),
		"priceFresh": fresh, "symbol": s.E.Cfg.TradeAsset, "chain": s.E.Cfg.Chain,
		"fees":   map[string]any{"bps": feeBps, "routedUsd": round2(routed), "earnedUsd": round2(routed * feeBps / 10000), "paper": s.E.Cfg.Paper},
		"social": map[string]any{"signalBot": s.E.Cfg.DiscordWebhook != "" || s.E.Cfg.TelegramToken != "", "rss": "/signals.rss"},
		"limits": map[string]any{"maxTradeUsd": s.E.Cfg.MaxTradeUSD, "minTradeUsd": s.E.Cfg.MinTradeUSD, "maxPositionPct": s.E.Cfg.MaxPositionPct, "approvalThresholdUsd": s.E.Cfg.ApprovalThresholdUSD,
			"maxApprovalUsd": s.E.Cfg.MaxApprovalUSD, "maxPriceDeviationPct": s.E.Cfg.MaxPriceDeviationPct, "maxAgentDrawdownPct": s.E.Cfg.MaxAgentDrawdownPct,
			"maxEntryCostPct": s.E.Cfg.MaxEntryCostPct, "maxSlippage": s.E.Cfg.MaxSlippage, "maxTradesPerDay": s.E.Cfg.MaxTradesPerDay,
			"netTargets": s.E.Cfg.NetTargets, "breakEvenStop": s.E.Cfg.BreakEvenStop,
			"maxMovePct": s.E.Cfg.MaxMovePct, "moveMinutes": int(time.Duration(s.E.Cfg.MoveChecks) * s.E.Cfg.PriceEvery / time.Minute),
			"maxLossStreak": s.E.Cfg.MaxLossStreak, "lossStreakPauseMin": int(s.E.Cfg.LossStreakPause / time.Minute),
			"dailyLossLimitUsd": s.E.Cfg.DailyLossLimitUSD, "maxHoldHours": s.E.Cfg.MaxHoldHours},
	})
}

func (s *Server) follows(w http.ResponseWriter, r *http.Request) {
	var out []store.Follow
	for id := range s.E.AgentMap() {
		out = append(out, s.E.Store.Followers(id)...)
	}
	writeJSON(w, 200, map[string]any{"follows": out})
}

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

var addrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// walletsList is every sub-wallet with balances, profile and limits.
func (s *Server) walletsList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var out []engine.WalletView
	for _, wl := range s.E.Wallets.List("") {
		out = append(out, s.E.Wallet(ctx, wl, false))
	}
	writeJSON(w, 200, map[string]any{"wallets": out, "mode": s.E.Cfg.ModeName()})
}

// walletGet adds the onchain reads (ETH for gas, Flash allowances).
func (s *Server) walletGet(w http.ResponseWriter, r *http.Request) {
	wl, ok := s.E.Wallets.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, 200, map[string]any{"wallet": s.E.Wallet(ctx, wl, true)})
}

// walletCreate makes a new sub-wallet for a person who copies or invests
// with an agent. The payout address is fixed here, once.
func (s *Server) walletCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID             string  `json:"id"`
		Owner          string  `json:"owner"`
		AgentID        string  `json:"agentId"`
		Mode           string  `json:"mode"` // invest | copy
		Payout         string  `json:"payout"`
		AllocationPct  float64 `json:"allocationPct"`
		MaxPerTradeUSD float64 `json:"maxPerTradeUsd"`
		DailyCapUSD    float64 `json:"dailyCapUsd"`
		LossLimitPct   float64 `json:"lossLimitPct"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	in.Owner = strings.TrimSpace(in.Owner)
	switch {
	case in.Owner == "" || len(in.Owner) > 40:
		writeErr(w, 400, "owner name is required (max 40 chars)")
		return
	case !addrRe.MatchString(in.Payout):
		writeErr(w, 400, "payout must be the owner's own 0x address on Base (withdrawals can only go there, and it cannot be changed later)")
		return
	case in.Mode != "invest" && in.Mode != "copy":
		writeErr(w, 400, "mode must be invest or copy")
		return
	}
	if _, ok := s.E.AgentMap()[in.AgentID]; !ok {
		writeErr(w, 400, "unknown agent")
		return
	}
	if s.E.Wallets.IsArenaAddress(in.Payout) {
		writeErr(w, 400, "payout cannot be one of the arena's own wallets")
		return
	}
	if in.ID == "" {
		in.ID = slug(in.Owner) + "-" + in.AgentID
	}
	f, err := s.followFrom(in.ID, in.AgentID, in.Mode, in.AllocationPct, in.MaxPerTradeUSD, in.DailyCapUSD)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.LossLimitPct < 0 || in.LossLimitPct > 90 {
		writeErr(w, 400, "lossLimitPct must be between 0 and 90")
		return
	}
	nw, err := s.E.Wallets.Create(in.ID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.E.Store.PutProfile(store.WalletProfile{ID: nw.ID, Owner: in.Owner, Payout: in.Payout, LossLimitPct: in.LossLimitPct})
	f.FollowerID = nw.ID
	s.E.Store.PutFollow(f)
	_ = s.E.Store.Save()
	verb := "is investing with"
	if in.Mode == "copy" {
		verb = "is copying"
	}
	s.E.Feed.Publish(feed.Post{Kind: "follow", AgentID: in.AgentID, Text: fmt.Sprintf("%s %s %s from a new sub-wallet (max $%.2f per trade, $%.2f a day)", in.Owner, verb, s.E.AgentMap()[in.AgentID].Name, f.MaxPerTradeUSD, f.DailyCapUSD)})
	s.Log.Info("sub-wallet created", "id", nw.ID, "address", nw.Address(), "agent", in.AgentID, "mode", in.Mode)
	writeJSON(w, 201, map[string]any{"wallet": s.E.Wallet(r.Context(), nw, false),
		"fund": fundHint(s, nw.Address())})
}

// fundHint: with a gas tank people send only USDC.
func fundHint(s *Server, addr string) string {
	if _, ok := s.E.Wallets.GasTank(); ok {
		return fmt.Sprintf("Send USDC on the Base network to %s (no ETH needed: the arena's gas tank pays gas)", addr)
	}
	return fmt.Sprintf("Send USDC (and ~$0.30 of ETH for approvals) on the Base network to %s", addr)
}

func slug(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 16 {
		out = out[:16]
	}
	if out == "" {
		out = "user"
	}
	return out
}

// followFrom validates follow limits against the arena's caps.
func (s *Server) followFrom(walletID, agentID, mode string, alloc, maxTrade, daily float64) (*store.Follow, error) {
	if alloc < 0 || alloc > s.E.Cfg.MaxPositionPct {
		return nil, fmt.Errorf("allocationPct must be 0-%.0f", s.E.Cfg.MaxPositionPct)
	}
	if maxTrade <= 0 || maxTrade > s.E.Cfg.MaxTradeUSD {
		maxTrade = s.E.Cfg.MaxTradeUSD
	}
	if daily <= 0 {
		daily = 3 * maxTrade
	}
	if daily < maxTrade {
		return nil, errors.New("dailyCapUsd must be at least maxPerTradeUsd")
	}
	return &store.Follow{FollowerID: walletID, AgentID: agentID, Mode: mode, AllocationPct: alloc, MaxPerTradeUSD: maxTrade,
		DailyCapUSD: daily, Active: true, CreatedAt: time.Now().UTC()}, nil
}

// walletLimits changes a wallet's limits / agent link. Payout is untouched.
func (s *Server) walletLimits(w http.ResponseWriter, r *http.Request) {
	wl, ok := s.E.Wallets.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	var in struct {
		AgentID        string   `json:"agentId"`
		Mode           string   `json:"mode"`
		AllocationPct  float64  `json:"allocationPct"`
		MaxPerTradeUSD float64  `json:"maxPerTradeUsd"`
		DailyCapUSD    float64  `json:"dailyCapUsd"`
		LossLimitPct   *float64 `json:"lossLimitPct"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.LossLimitPct != nil {
		if *in.LossLimitPct < 0 || *in.LossLimitPct > 90 {
			writeErr(w, 400, "lossLimitPct must be between 0 and 90")
			return
		}
		s.E.Store.UpdateProfile(wl.ID, func(p *store.WalletProfile) { p.LossLimitPct = *in.LossLimitPct })
	}
	if in.AgentID != "" {
		if wl.Role != wallet.RoleFollower {
			writeErr(w, 400, "only follower wallets copy or invest")
			return
		}
		if _, ok := s.E.AgentMap()[in.AgentID]; !ok {
			writeErr(w, 400, "unknown agent")
			return
		}
		if in.Mode != "invest" && in.Mode != "copy" {
			writeErr(w, 400, "mode must be invest or copy")
			return
		}
		f, err := s.followFrom(wl.ID, in.AgentID, in.Mode, in.AllocationPct, in.MaxPerTradeUSD, in.DailyCapUSD)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		for _, old := range s.E.Store.FollowsOf(wl.ID) { // one agent per sub-wallet
			if old.AgentID != in.AgentID {
				s.E.Store.Unfollow(wl.ID, old.AgentID)
			}
		}
		s.E.Store.PutFollow(f)
	}
	writeJSON(w, 200, map[string]any{"wallet": s.E.Wallet(r.Context(), wl, false)})
}

func (s *Server) walletPause(w http.ResponseWriter, r *http.Request) {
	wl, ok := s.E.Wallets.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	pause := r.PathValue("action") == "pause"
	if r.PathValue("action") != "pause" && r.PathValue("action") != "resume" {
		writeErr(w, 404, "unknown action")
		return
	}
	p := s.E.Store.UpdateProfile(wl.ID, func(p *store.WalletProfile) {
		p.Paused = pause
		p.PausedReason = ""
		if pause {
			p.PausedReason = "paused by the operator"
		}
	})
	verb := "resumed"
	if pause {
		verb = "paused"
	}
	s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("Wallet %s %s", wl.ID, verb)})
	writeJSON(w, 200, map[string]any{"profile": p})
}

func (s *Server) walletWithdraw(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token  string  `json:"token"`
		Amount float64 `json:"amount"` // 0 = all
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tx, amt, err := s.E.Withdraw(ctx, r.PathValue("id"), in.Token, in.Amount)
	if err != nil {
		code := 400
		if strings.Contains(err.Error(), "insufficient funds") || strings.Contains(err.Error(), "gas") {
			err = fmt.Errorf("%w (the wallet needs a little ETH on Base to pay gas)", err)
		}
		writeErr(w, code, err.Error())
		return
	}
	p, _ := s.E.Store.Profile(r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true, "tx": tx, "amount": amt, "to": p.Payout})
}

func (s *Server) walletRevoke(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	txs, err := s.E.RevokeAll(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "txs": txs})
}

// walletOrders is the wallet's order history straight from Flash (GET /orders).
func (s *Server) walletOrders(w http.ResponseWriter, r *http.Request) {
	wl, ok := s.E.Wallets.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	if s.E.Cfg.DryRun {
		writeJSON(w, 200, map[string]any{"orders": []any{}, "note": "dry run sends no orders"})
		return
	}
	orders, next, err := s.E.Flash.ListOrders(r.Context(), wl.Address(), nil, 25, r.URL.Query().Get("pageToken"))
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	type row struct {
		ID, Type, Side, Status, Reason, Bracket string
		Qty, Target, Contra, Price              float64
		PlacedAt                                string
	}
	var out []row
	for _, o := range orders {
		rw := row{ID: o.OrderID, Type: o.OrderType, Side: o.Side, Status: strings.TrimPrefix(o.Status, "ORDER_STATUS_"),
			Reason: o.Reason(), Qty: o.Qty.Float(), Target: o.FilledTarget(), Contra: o.FilledContra(), Price: o.AvgNotionalPrice(), PlacedAt: o.PlacedAt}
		if o.AttachedBracket != nil {
			rw.Bracket = o.AttachedBracket.Status
		}
		out = append(out, rw)
	}
	writeJSON(w, 200, map[string]any{"orders": out, "nextPageToken": next})
}

// paperTopUp adds mock USDC to a wallet (paper mode only).
func (s *Server) paperTopUp(w http.ResponseWriter, r *http.Request) {
	if !s.E.Cfg.Paper || s.E.Venue == nil {
		writeErr(w, 409, "top-ups only exist in paper mode")
		return
	}
	wl, ok := s.E.Wallets.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	var in struct {
		USD float64 `json:"usd"`
	}
	if err := decode(r, &in); err != nil || in.USD <= 0 || in.USD > 1000 {
		writeErr(w, 400, "usd must be between 0 and 1000")
		return
	}
	s.E.Venue.Deposit(wl.Address(), s.E.Cfg.USDC, in.USD)
	s.E.InvalidateBalance(wl.ID)
	s.E.Store.AddFlow(wl.ID, in.USD) // a deposit, not a profit
	s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("[paper] %s got $%.2f of paper USDC", wl.ID, in.USD)})
	writeJSON(w, 200, map[string]any{"wallet": s.E.Wallet(r.Context(), wl, false)})
}

// testTrade is the operator's small real-money (or paper) check: one bracket
// order of TEST_TRADE_USD from one wallet, nothing copied.
func (s *Server) testTrade(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WalletID string `json:"walletId"`
	}
	if err := decode(r, &in); err != nil || in.WalletID == "" {
		writeErr(w, 400, "walletId is required")
		return
	}
	wl, ok := s.E.Wallets.Get(in.WalletID)
	if !ok {
		writeErr(w, 404, "wallet not found")
		return
	}
	px := s.E.Prices.Last()
	if px <= 0 {
		writeErr(w, 503, "no price yet")
		return
	}
	agentID := wl.ID
	if _, isAgent := s.E.AgentMap()[wl.ID]; !isAgent {
		agentID = ""
		for _, f := range s.E.Store.FollowsOf(wl.ID) {
			agentID = f.AgentID
		}
		if agentID == "" {
			for id := range s.E.AgentMap() {
				agentID = id
				break
			}
		}
	}
	cfg := s.E.Cfg
	intent := referee.Intent{AgentID: agentID, WalletID: wl.ID, OrderType: "market+bracket", Token: cfg.TradeAsset, Price: px,
		TakeProfit: round2(px * (1 + cfg.TestTPPct/100)), StopLoss: round2(px * (1 - cfg.TestSLPct/100)),
		Reason: "operator test trade", Test: true}
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

// round2 rounds half away from zero, for losses too (-0.046 -> -0.05).
func round2(x float64) float64 { return math.Round(x*100) / 100 }

func (s *Server) emergencyStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	n, errs := s.E.Halt(ctx, "operator")
	writeJSON(w, 200, map[string]any{"ok": true, "cancelled": n, "errors": errs})
}

func (s *Server) resumeAll(w http.ResponseWriter, r *http.Request) {
	s.E.Resume()
	writeJSON(w, 200, map[string]any{"ok": true})
}

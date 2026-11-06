package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/store"
)

// tokens lists the coins a plan can trade and the strategies it can use.
func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"tokens": s.E.TokenPrices(), "strategies": engine.PlanStrategies, "styles": engine.Styles,
		"limits": map[string]any{"maxTradeUsd": s.E.Cfg.MaxTradeUSD, "maxTradesPerDay": s.E.Cfg.MaxTradesPerDay,
			"minTakeProfitPct": 0.6, "maxStopLossPct": 5, "maxSlippagePct": 5, "maxDurationHours": 720},
		"mode": s.E.Cfg.ModeName()})
}

type planView struct {
	store.Plan
	Stats   engine.PlanStats `json:"stats"`
	Address string           `json:"address"`
	USDC    float64          `json:"usdc"`
	Holding float64          `json:"holding"` // amount of the plan's coin held
	Price   float64          `json:"price"`
}

func (s *Server) planView(ctx context.Context, p store.Plan, withBal bool) (planView, []store.Trade) {
	st, trades := s.E.PlanStats(p.ID)
	v := planView{Plan: p, Stats: st}
	if wl, ok := s.E.Wallets.Get(p.WalletID); ok {
		v.Address = wl.Address()
		if withBal {
			v.USDC, _, _ = s.E.Balances(ctx, wl, 15*time.Second)
			if tok, ok := s.E.Cfg.TokenBySymbol(p.Token); ok {
				v.Holding, _ = s.E.TokenBalance(ctx, wl, tok.Address, 15*time.Second)
				v.Price = s.E.TokenPrice(tok.Address)
			}
		}
	}
	return v, trades
}

func (s *Server) plansList(w http.ResponseWriter, r *http.Request) {
	out := []planView{}
	for _, p := range s.E.Store.Plans() {
		v, _ := s.planView(r.Context(), p, false)
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"plans": out, "mode": s.E.Cfg.ModeName()})
}

func (s *Server) planGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.E.Store.Plan(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "plan not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	v, trades := s.planView(ctx, p, true)
	if len(trades) > 50 {
		trades = trades[:50]
	}
	writeJSON(w, 200, map[string]any{"plan": v, "trades": trades, "strategy": engine.PlanStrategies[p.Strategy]})
}

func (s *Server) planCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string  `json:"name"`
		Owner           string  `json:"owner"`
		Payout          string  `json:"payout"`
		Token           string  `json:"token"`
		Strategy        string  `json:"strategy"`
		IntervalMin     int     `json:"intervalMin"`
		TradeUSD        float64 `json:"tradeUsd"`
		BudgetUSD       float64 `json:"budgetUsd"`
		TakeProfitPct   float64 `json:"takeProfitPct"`
		StopLossPct     float64 `json:"stopLossPct"`
		MaxSlippagePct  float64 `json:"maxSlippagePct"`
		MaxTradesPerDay int     `json:"maxTradesPerDay"`
		LossLimitPct    float64 `json:"lossLimitPct"`
		DurationHours   float64 `json:"durationHours"`
		ExitAtEnd       bool    `json:"exitAtEnd"`
		Style           string  `json:"style"`
		LimitPct        float64 `json:"limitPct"`
		StopPct         float64 `json:"stopPct"`
		TwapMinutes     int     `json:"twapMinutes"`
		Protect         string  `json:"protect"`
		TrailPct        float64 `json:"trailPct"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := s.E.Cfg
	in.Name, in.Owner = strings.TrimSpace(in.Name), strings.TrimSpace(in.Owner)
	tok, tokOK := cfg.TokenBySymbol(in.Token)
	if in.MaxTradesPerDay == 0 {
		in.MaxTradesPerDay = cfg.MaxTradesPerDay
	}
	if in.MaxSlippagePct == 0 {
		in.MaxSlippagePct = 1
	}
	if in.Style == "" {
		in.Style = "market"
	}
	fail := func(msg string) { writeErr(w, 400, msg) }
	switch {
	case in.Name == "" || len(in.Name) > 40:
		fail("give the plan a name (max 40 chars)")
	case in.Owner == "" || len(in.Owner) > 40:
		fail("owner name is required (max 40 chars)")
	case !addrRe.MatchString(in.Payout):
		fail("payout must be your own 0x address on Base; withdrawals can only go there")
	case !tokOK:
		fail("unknown coin " + in.Token)
	case s.E.TokenPrice(tok.Address) <= 0:
		fail("no live price for " + tok.Symbol + " yet; try again in a minute")
	case engine.PlanStrategies[in.Strategy] == "":
		fail("strategy must be auto, trend, dip, contrarian or interval")
	case in.Strategy == "interval" && (in.IntervalMin < 1 || in.IntervalMin > 1440):
		fail("interval must be 1-1440 minutes")
	case in.TradeUSD < 1 || in.TradeUSD > cfg.MaxTradeUSD:
		fail(fmt.Sprintf("money per trade must be $1-$%.0f", cfg.MaxTradeUSD))
	case in.BudgetUSD < in.TradeUSD || in.BudgetUSD > 10000:
		fail("budget must be at least one trade and at most $10,000")
	case in.TakeProfitPct < 0.6 || in.TakeProfitPct > 50:
		fail("take profit must be 0.6%-50%")
	case in.StopLossPct < 0.2 || in.StopLossPct > 5:
		fail("stop loss must be 0.2%-5% (arena rule: every trade has a stop within 5%)")
	case in.MaxSlippagePct < 0.1 || in.MaxSlippagePct > 5:
		fail("slippage must be 0.1%-5%")
	case in.MaxTradesPerDay < 1 || in.MaxTradesPerDay > cfg.MaxTradesPerDay:
		fail(fmt.Sprintf("trades per day must be 1-%d", cfg.MaxTradesPerDay))
	case in.LossLimitPct < 0 || in.LossLimitPct > 100:
		fail("loss limit must be 0-100% of the budget")
	case in.DurationHours < 0.25 || in.DurationHours > 720:
		fail("duration must be 15 minutes to 30 days")
	case engine.Styles[in.Style] == "":
		fail("order style must be market, limit, twap or stop")
	case in.Style == "limit" && (in.LimitPct < 0.1 || in.LimitPct > 10):
		fail("limit must be 0.1%-10% below the price")
	case in.Style == "stop" && (in.StopPct < 0.1 || in.StopPct > 10):
		fail("stop buy must trigger 0.1%-10% above the price")
	case in.Style == "twap" && (in.TwapMinutes < 5 || in.TwapMinutes > 1440):
		fail("DCA / TWAP must run 5-1440 minutes")
	case in.Protect == "trailing" && in.Style != "stop":
		fail("trailing stop protection is for stop buys")
	case in.Protect == "trailing" && (in.TrailPct < 0.5 || in.TrailPct > 20):
		fail("trailing stop must be 0.5%-20%")
	default:
		goto ok
	}
	return
ok:
	for _, wl := range s.E.Wallets.List("") {
		if strings.EqualFold(wl.Address(), in.Payout) {
			fail("payout cannot be one of the arena's own sub-wallets")
			return
		}
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	wid := "plan-" + slug(in.Owner)
	if len(wid) > 26 {
		wid = wid[:26]
	}
	wid += "-" + hex.EncodeToString(b)
	nw, err := s.E.Wallets.Create(wid)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	lossLimit := in.LossLimitPct
	if lossLimit == 0 {
		lossLimit = 100
	}
	now := time.Now().UTC()
	pid := make([]byte, 6)
	_, _ = rand.Read(pid)
	p := store.Plan{ID: "pln_" + hex.EncodeToString(pid),
		Name: in.Name, Owner: in.Owner, WalletID: nw.ID, Token: tok.Symbol, Strategy: in.Strategy, IntervalMin: in.IntervalMin,
		TradeUSD: in.TradeUSD, BudgetUSD: in.BudgetUSD, TakeProfitPct: in.TakeProfitPct, StopLossPct: in.StopLossPct,
		MaxSlippagePct: in.MaxSlippagePct, MaxTradesPerDay: in.MaxTradesPerDay, LossLimitPct: lossLimit, ExitAtEnd: in.ExitAtEnd,
		Style: in.Style, LimitPct: in.LimitPct, StopPct: in.StopPct, TwapMinutes: in.TwapMinutes, ProtectMode: in.Protect, TrailPct: in.TrailPct,
		StartsAt: now, EndsAt: now.Add(time.Duration(in.DurationHours * float64(time.Hour))), Status: "active", CreatedAt: now}
	s.E.Store.PutProfile(store.WalletProfile{ID: nw.ID, Owner: in.Owner, Payout: in.Payout})
	s.E.Store.PutPlan(p)
	if cfg.Paper && s.E.Venue != nil { // demo money: fund the plan's budget
		if have := s.E.Venue.Balance(nw.Address(), cfg.USDC); have < in.BudgetUSD {
			s.E.Venue.Deposit(nw.Address(), cfg.USDC, math.Ceil(in.BudgetUSD-have))
			s.E.Store.AddFlow(nw.ID, math.Ceil(in.BudgetUSD-have))
			s.E.InvalidateBalance(nw.ID)
		}
	}
	_ = s.E.Store.Save()
	s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%s started a plan \"%s\": %s with %s, $%.2f a trade, TP %.1f%% / SL %.1f%%, for %s",
		in.Owner, in.Name, tok.Symbol, in.Strategy, in.TradeUSD, in.TakeProfitPct, in.StopLossPct, humanDur(in.DurationHours))})
	go s.E.StepPlan(context.WithoutCancel(r.Context()), p.ID) // first look right away
	v, _ := s.planView(r.Context(), p, false)
	writeJSON(w, 201, map[string]any{"plan": v,
		"fund": fmt.Sprintf("Send USDC (and ~$0.30 of ETH for approvals) on the Base network to %s", nw.Address())})
}

func humanDur(h float64) string {
	switch {
	case h < 1:
		return fmt.Sprintf("%.0f minutes", h*60)
	case h < 48:
		return fmt.Sprintf("%.0f hours", h)
	}
	return fmt.Sprintf("%.0f days", h/24)
}

func (s *Server) planAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.E.Store.Plan(id)
	if !ok {
		writeErr(w, 404, "plan not found")
		return
	}
	switch a := r.PathValue("action"); a {
	case "pause", "resume", "stop":
		if p.Status == "ended" || p.Status == "stopped" {
			writeErr(w, 409, "plan is "+p.Status)
			return
		}
		status := map[string]string{"pause": "paused", "resume": "active", "stop": "stopped"}[a]
		p, _ = s.E.Store.UpdatePlan(id, func(x *store.Plan) { x.Status, x.StatusReason = status, "by the operator" })
		s.E.Feed.Publish(feed.Post{Kind: "follow", Text: fmt.Sprintf("%s's plan \"%s\" is %s", p.Owner, p.Name, status)})
	case "check":
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		s.E.StepPlan(ctx, id)
		p, _ = s.E.Store.Plan(id)
	default:
		writeErr(w, 404, "unknown action")
		return
	}
	v, _ := s.planView(r.Context(), p, true)
	writeJSON(w, 200, map[string]any{"plan": v})
}

package httpapi

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
)

// tradeChart is everything the trade page draws: the price since a little
// before the trade, its lines (entry, take profit, stop loss, break-even,
// exit), the moments that changed it, and the value right now if it is open.
func (s *Server) tradeChart(w http.ResponseWriter, r *http.Request) {
	t, ok := s.E.Store.Trade(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "trade not found")
		return
	}
	sym := t.Token
	if sym == "" {
		sym = s.E.Cfg.TradeAsset
	}
	addr := t.TokenAddr
	if addr == "" {
		addr = s.E.Cfg.WETH
	}
	open := t.Open()
	from := t.CreatedAt.Add(-15 * time.Minute)
	to := time.Now()
	if !open {
		to = t.UpdatedAt.Add(5 * time.Minute)
	}
	var pts []arena.TapePoint
	if s.E.Tape != nil {
		pts = s.E.Tape.Range(sym, from, to, 480)
	}
	now := s.E.TokenPrice(addr)
	entry := t.FillPrice
	if entry <= 0 {
		entry = t.QuotedPrice
	}
	if entry <= 0 {
		entry = t.Entry
	}
	type event struct {
		T     int64  `json:"t"`
		Kind  string `json:"kind"`
		Label string `json:"label"`
	}
	label := map[string]string{"submitted": "Order sent", "filled": "Bought", "stop_moved": "Stop moved", "breakeven": "Stop to break-even",
		"exit_submitted": "Sell sent", "closed": "Closed", "bracket_cancelled": "Bracket cancelled", "unprotected": "Unprotected"}
	var events []event
	for _, st := range t.Timeline {
		l, ok := label[st.Step]
		if !ok && !strings.Contains(st.Step, "stop") {
			continue
		}
		if l == "" {
			l = strings.ReplaceAll(st.Step, "_", " ")
		}
		events = append(events, event{T: st.At.UnixMilli(), Kind: st.Step, Label: l})
	}
	out := map[string]any{"token": sym, "points": pts, "open": open, "status": t.Status, "now": now,
		"entry": entry, "takeProfit": t.TakeProfit, "stopLoss": t.StopLoss, "breakEven": t.BreakEvenPrice, "exit": t.ExitPrice,
		"createdAt": t.CreatedAt.UnixMilli(), "events": events}
	// Value if sold at the current price (before Flash's sell cost).
	if (t.Status == "filled" || t.Status == "unprotected") && t.WETHAmount > 0 && t.USDCSpent > 0 && now > 0 {
		v := t.WETHAmount * now
		out["valueUsd"] = math.Round(v*1e4) / 1e4
		out["pnlUsd"] = math.Round((v-t.USDCSpent)*1e4) / 1e4
		out["pnlPct"] = math.Round((v/t.USDCSpent-1)*1e4) / 100
		if t.TakeProfit > 0 {
			out["toTargetPct"] = math.Round((t.TakeProfit/now-1)*1e4) / 100
		}
		if t.StopLoss > 0 {
			out["toStopPct"] = math.Round((now/t.StopLoss-1)*1e4) / 100
		}
	}
	writeJSON(w, 200, out)
}

// backtest runs agent rules over real past prices (public; rate limited).
func (s *Server) backtest(w http.ResponseWriter, r *http.Request) {
	if !allow("backtest", r, 20, time.Minute) {
		writeErr(w, 429, "too many backtests from here; wait a minute")
		return
	}
	var in struct {
		Strategy      string  `json:"strategy"`
		Token         string  `json:"token"`
		Fast          int     `json:"fast"`
		Slow          int     `json:"slow"`
		Window        int     `json:"window"`
		DipPct        float64 `json:"dipPct"`
		Streak        int     `json:"streak"`
		Trend         int     `json:"trend"`
		TakeProfitPct float64 `json:"takeProfitPct"`
		StopLossPct   float64 `json:"stopLossPct"`
		SizePct       float64 `json:"sizePct"`
		SizeUSD       float64 `json:"sizeUsd"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.SizeUSD < 0 || in.SizeUSD > 1000 {
		writeErr(w, 400, "sizeUsd: 0-1000")
		return
	}
	rep, err := s.E.Backtest(r.Context(), arena.StrategyParams{Kind: in.Strategy, Fast: in.Fast, Slow: in.Slow, Window: in.Window,
		DipPct: in.DipPct, Streak: in.Streak, Trend: in.Trend, TPPct: in.TakeProfitPct, SLPct: in.StopLossPct, SizePct: in.SizePct},
		strings.ToUpper(strings.TrimSpace(in.Token)), in.SizeUSD)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, rep)
}

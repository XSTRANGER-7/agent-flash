package httpapi

// Trade or copy from your own wallet (self-custody): POST /v1/wallet/quote
// returns the checked FlashOrder payloads to sign in the browser wallet;
// POST /v1/wallet/order submits the signatures. No key ever reaches us.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/referee"
)

// allowQuote: 12 self-custody quotes a minute per client IP (the shared
// limiter, which trusts a forwarded IP only from the local tunnel).
func allowQuote(r *http.Request) bool {
	return allow("quote", r, 12, time.Minute)
}

func (s *Server) selfQuote(w http.ResponseWriter, r *http.Request) {
	if !allowQuote(r) {
		writeErr(w, 429, "too many quotes; wait a minute")
		return
	}
	var in struct {
		Address        string  `json:"address"`
		Token          string  `json:"token"`
		Style          string  `json:"style"`
		SizeUSD        float64 `json:"sizeUsd"`
		TakeProfitPct  float64 `json:"takeProfitPct"`
		StopLossPct    float64 `json:"stopLossPct"`
		LimitPct       float64 `json:"limitPct"`
		TwapMinutes    int     `json:"twapMinutes"`
		MaxSlippagePct float64 `json:"maxSlippagePct"`
		CopyOf         string  `json:"copyOf"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !addrRe.MatchString(in.Address) {
		writeErr(w, 400, "connect a wallet first")
		return
	}
	if in.SizeUSD < 1 || in.SizeUSD > s.E.Cfg.MaxTradeUSD {
		writeErr(w, 400, "size must be $1-$"+strconv.FormatFloat(s.E.Cfg.MaxTradeUSD, 'f', 0, 64))
		return
	}
	intent := referee.Intent{Token: firstNonEmpty(in.Token, s.E.Cfg.TradeAsset), SizeUSD: in.SizeUSD, Style: firstNonEmpty(in.Style, "market"),
		TwapMinutes: in.TwapMinutes, Reason: "from my own wallet"}
	tpPct, slPct := in.TakeProfitPct, in.StopLossPct
	if in.CopyOf != "" { // copy a signal: same coin and the same % targets
		src, ok := s.E.Store.Trade(in.CopyOf)
		if !ok || src.Entry <= 0 {
			writeErr(w, 404, "signal not found")
			return
		}
		intent.Token, intent.AgentID, intent.CopyOf = tokenOr(src.Token), src.AgentID, src.ID
		tpPct, slPct = (src.TakeProfit/src.Entry-1)*100, (1-src.StopLoss/src.Entry)*100
		intent.Reason = "copy of " + s.name(*src)
	}
	if tpPct == 0 {
		tpPct = 2
	}
	if slPct == 0 {
		slPct = 1
	}
	tok, ok := s.E.Cfg.TokenBySymbol(intent.Token)
	if !ok {
		writeErr(w, 400, "unknown coin")
		return
	}
	intent.Token = tok.Symbol
	px := s.E.TokenPrice(tok.Address)
	if px <= 0 {
		writeErr(w, 503, "no live price yet")
		return
	}
	intent.Price = px
	entry := px
	if intent.Style == "limit" {
		intent.EntryLimit = round2(px * (1 - in.LimitPct/100))
		entry = intent.EntryLimit
	}
	intent.TakeProfit, intent.StopLoss = round2(entry*(1+tpPct/100)), round2(entry*(1-slPct/100))
	if in.MaxSlippagePct > 0 {
		intent.MaxSlippage = strconv.FormatFloat(in.MaxSlippagePct/100, 'f', -1, 64)
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	p, d, err := s.E.SelfPrepare(ctx, in.Address, intent)
	switch {
	case err != nil:
		writeErr(w, 502, err.Error())
	case !d.OK:
		writeJSON(w, 422, map[string]any{"decision": d})
	default:
		writeJSON(w, 200, map[string]any{"quote": p.External, "mode": s.E.Cfg.ModeName(), "chainId": s.E.Cfg.ChainID,
			"summary": map[string]any{"token": tok.Symbol, "sizeUsd": in.SizeUSD, "style": intent.Style, "mark": p.MarkPrice, "costPct": p.CostPct,
				"costUsd": p.CostUSD, "takeProfit": p.TakeProfit, "stopLoss": p.StopLoss, "entryLimit": intent.EntryLimit}})
	}
}

func (s *Server) selfOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		QuoteID          string `json:"quoteId"`
		Address          string `json:"address"`
		EntrySignature   string `json:"entrySignature"`
		BracketSignature string `json:"bracketSignature"`
	}
	if err := decode(r, &in); err != nil || in.QuoteID == "" || !addrRe.MatchString(in.Address) || in.EntrySignature == "" {
		writeErr(w, 400, "quoteId, address and entrySignature are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := s.E.SelfSubmit(ctx, in.QuoteID, in.Address, in.EntrySignature, in.BracketSignature)
	switch {
	case err != nil:
		writeErr(w, 400, err.Error())
	case !res.Decision.OK:
		writeJSON(w, 422, res)
	case res.Error != "":
		code := 502 // Flash or the network failed
		if strings.Contains(res.Error, "signature") || strings.Contains(res.Error, "quote") {
			code = 400 // the request itself is wrong: a bad signature or a stale quote
		}
		writeJSON(w, code, res)
	default:
		writeJSON(w, 201, res)
	}
}

// selfWallet is a person's own wallet as the arena sees it (public data).
func (s *Server) selfWallet(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("address")
	if !addrRe.MatchString(addr) {
		writeErr(w, 400, "not an address")
		return
	}
	out := map[string]any{"address": addr, "mode": s.E.Cfg.ModeName(), "trades": []any{}}
	wl, ok := s.E.Wallets.SelfFor(addr)
	if !ok && strings.EqualFold(sessionAddr(r), addr) {
		// The signed-in owner looking at their own wallet: watch it so its
		// balances show before a first trade (a stranger's lookup never
		// registers anything).
		if nw, err := s.E.Wallets.Watch(addr); err == nil {
			s.E.Store.AddSelfWallet(nw.ID, nw.Address())
			wl, ok = nw, true
		}
	}
	if ok {
		id := wl.ID
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		v := s.E.Wallet(ctx, wl, false)
		var mine []any
		for _, t := range s.E.Store.Trades("", 0) {
			if t.WalletID == id {
				mine = append(mine, t)
			}
		}
		out["usdc"], out["weth"], out["equityUsd"], out["pnlUsd"] = v.USDC, v.WETH, v.EquityUSD, v.PnLUSD
		if mine != nil {
			out["trades"] = mine
		}
	}
	writeJSON(w, 200, out)
}

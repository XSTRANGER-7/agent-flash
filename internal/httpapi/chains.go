package httpapi

// GET /v1/chains: every chain Flash executes on, with live data (gas-coin
// price from a real Flash quote, Flash's all-in cost, gas and block from
// the chain's RPC). GET /v1/wallet/{address}/chains: what an address holds
// on each chain an own wallet can trade on (read onchain, public data).

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/engine"
)

func (s *Server) chains(w http.ResponseWriter, r *http.Request) {
	home := s.E.Cfg.Home()
	trade := []string{}
	for _, ch := range s.E.Cfg.Chains {
		if ch.Tradeable() {
			trade = append(trade, ch.Key)
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, 200, map[string]any{"chains": s.E.Chains(), "home": home.Key, "tradeChains": trade,
		"source": map[string]string{"price": "Flash quote: $25 USDC -> gas coin (mark, before costs)", "gas": "eth_gasPrice on each chain's RPC"}})
}

// chainInfo is a chain as the wallet needs it to switch networks.
type chainInfo struct {
	config.Chain
	Tradeable bool `json:"tradeable"`
}

func (s *Server) walletChains(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("address")
	if !addrRe.MatchString(addr) {
		writeErr(w, 400, "not an address")
		return
	}
	if !allow("wchains", r, 20, time.Minute) {
		writeErr(w, 429, "too many balance reads; wait a minute")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := []engine.ChainBalance{}
	info := []chainInfo{}
	for _, ch := range s.E.Cfg.Chains {
		if !ch.Tradeable() {
			continue
		}
		info = append(info, chainInfo{Chain: ch, Tradeable: true})
		wg.Add(1)
		go func(ch config.Chain) {
			defer wg.Done()
			b, err := s.E.ChainBalances(ctx, ch, addr)
			if err != nil {
				b = engine.ChainBalance{Chain: ch.Key, Error: "could not read " + ch.Name + " right now"}
			}
			mu.Lock()
			out = append(out, b)
			mu.Unlock()
		}(ch)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"address": addr, "home": s.E.Cfg.Chain, "mode": s.E.Cfg.ModeName(), "chains": info, "balances": out})
}

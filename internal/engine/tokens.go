package engine

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// Coins other than WETH are priced by a QuoteFeed (Flash quotes); WETH by
// the onchain pool feed. Keyed by lower-case token address.

func (e *Engine) isWETH(addr string) bool { return addr == "" || strings.EqualFold(addr, e.Cfg.WETH) }

func (e *Engine) quoteFeed(addr string) *arena.QuoteFeed {
	if e.TokenFeeds == nil {
		return nil
	}
	return e.TokenFeeds[strings.ToLower(addr)]
}

func (e *Engine) tokenPrice(addr string) float64 {
	if e.isWETH(addr) {
		return e.Prices.Last()
	}
	if f := e.quoteFeed(addr); f != nil {
		return f.Last()
	}
	return 0
}

func (e *Engine) tokenHistory(addr string, maxAge time.Duration) ([]float64, bool) {
	if e.isWETH(addr) {
		return e.Prices.Snapshot(maxAge)
	}
	if f := e.quoteFeed(addr); f != nil {
		return f.Snapshot(maxAge)
	}
	return nil, false
}

// tokenMeta is (liquidity USD, Flash risk flag) for the referee.
func (e *Engine) tokenMeta(ctx context.Context, addr string) (float64, bool) {
	if e.isWETH(addr) {
		if _, _, ok := e.Prices.Meta(); !ok {
			e.Prices.Refresh(ctx)
		}
		l, r, _ := e.Prices.Meta()
		return l, r
	}
	if f := e.quoteFeed(addr); f != nil {
		l, r, _ := f.Meta()
		return l, r
	}
	return 0, false
}

// TokenBalance reads one token from the (cached) Flash balances.
func (e *Engine) TokenBalance(ctx context.Context, w *wallet.Wallet, addr string, maxAge time.Duration) (float64, error) {
	if addr == "" {
		addr = e.Cfg.WETH
	}
	if _, _, err := e.Balances(ctx, w, maxAge); err != nil {
		return 0, err
	}
	e.balMu.Lock()
	defer e.balMu.Unlock()
	return flash.BalanceOf(e.bals[w.ID].all, e.Cfg.Chain, addr), nil
}

// holdingsUSD values every configured token in a wallet (equity, views).
func (e *Engine) holdingsUSD(w *wallet.Wallet) float64 {
	e.balMu.Lock()
	c := e.bals[w.ID]
	e.balMu.Unlock()
	sum := 0.0
	for _, t := range e.Cfg.Tokens {
		if amt := flash.BalanceOf(c.all, e.Cfg.Chain, t.Address); amt > 0 {
			sum += amt * e.tokenPrice(t.Address)
		}
	}
	return sum
}

// TokenPrices lists the tradeable coins with their current price.
func (e *Engine) TokenPrices() []map[string]any {
	var out []map[string]any
	for _, t := range e.Cfg.Tokens {
		src, liq, liqSrc := "uniswap-v3 pool", e.Prices.Status().LiquidityUSD, e.Prices.Status().LiquiditySource
		if !e.isWETH(t.Address) {
			src, liq, liqSrc = "Flash quotes", 0, ""
			if f := e.quoteFeed(t.Address); f != nil {
				liq, _, _ = f.Meta()
				liqSrc = f.LiquiditySource()
			}
		}
		out = append(out, map[string]any{"symbol": t.Symbol, "address": t.Address, "decimals": t.Decimals, "price": e.tokenPrice(t.Address), "source": src,
			"liquidityUsd": math.Round(liq), "liquiditySource": liqSrc})
	}
	return out
}

func fmtPx(x float64) string {
	if x >= 1000 {
		return strconv.FormatFloat(x, 'f', 0, 64)
	}
	return strconv.FormatFloat(x, 'f', 2, 64)
}

func tokenSym(t store.Trade) string {
	if t.Token != "" {
		return t.Token
	}
	return "WETH"
}

// tradeToken is the token a trade bought (older trades: WETH).
func (e *Engine) tradeToken(t store.Trade) config.Token {
	if t.TokenAddr != "" {
		if tok, ok := e.Cfg.TokenByAddress(t.TokenAddr); ok {
			return tok
		}
	}
	return config.Token{Symbol: e.Cfg.TradeAsset, Address: e.Cfg.WETH, Decimals: 18}
}

// TokenPrice is the reference USD price of a token address (0 = unknown).
func (e *Engine) TokenPrice(addr string) float64 { return e.tokenPrice(addr) }

// tokenBySymbol is Cfg.TokenBySymbol with the agents' asset always known.
func (e *Engine) tokenBySymbol(sym string) (config.Token, bool) {
	if t, ok := e.Cfg.TokenBySymbol(sym); ok {
		return t, true
	}
	if sym == "" || strings.EqualFold(sym, e.Cfg.TradeAsset) {
		return config.Token{Symbol: e.Cfg.TradeAsset, Address: e.Cfg.WETH, Decimals: 18}, true
	}
	return config.Token{}, false
}

// tokenPriceOrOne values a withdrawn token in USD (USDC = 1).
func (e *Engine) tokenPriceOrOne(addr string) float64 {
	if strings.EqualFold(addr, e.Cfg.USDC) {
		return 1
	}
	if p := e.tokenPrice(addr); p > 0 {
		return p
	}
	return 1
}

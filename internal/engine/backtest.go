package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/store"
)

// BacktestReport is a backtest plus where its prices and costs came from.
type BacktestReport struct {
	arena.BacktestResult
	Token           string    `json:"token"`
	Prices          []float64 `json:"prices"`
	StartAt         time.Time `json:"startAt"`
	StepSec         int       `json:"stepSec"`
	Hours           float64   `json:"hours"`
	PriceSource     string    `json:"priceSource"`
	SizeUSD         float64   `json:"sizeUsd"`
	CostPerOrderUSD float64   `json:"costPerOrderUsd"`
	CostSource      string    `json:"costSource"`
	CostPct         float64   `json:"costPct"`      // Flash's entry cost at this size, %
	CostLimitPct    float64   `json:"costLimitPct"` // the cost limit that applies
	CostBlocked     bool      `json:"costBlocked"`  // every entry would be refused by the cost limit
}

type btCache struct {
	mu     sync.Mutex
	prices map[string][]float64
	at     map[string]time.Time
	cost   map[string]float64
	costAt map[string]time.Time
}

var bt = &btCache{prices: map[string][]float64{}, at: map[string]time.Time{}, cost: map[string]float64{}, costAt: map[string]time.Time{}}

// history returns the past prices of a coin at the price-check cadence:
// WETH from the Uniswap pool's own onchain oracle (a few hours), other coins
// from the recorded price tape. Cached for a minute.
func (e *Engine) history(ctx context.Context, sym string) ([]float64, string, error) {
	sym = strings.ToUpper(sym)
	bt.mu.Lock()
	if p, ok := bt.prices[sym]; ok && time.Since(bt.at[sym]) < time.Minute {
		bt.mu.Unlock()
		return p, "cache", nil
	}
	bt.mu.Unlock()
	step := e.Cfg.PriceEvery
	if step < time.Second {
		step = 15 * time.Second
	}
	var prices []float64
	src := ""
	tok, ok := e.tokenBySymbol(sym)
	if !ok {
		return nil, "", fmt.Errorf("unknown coin %s", sym)
	}
	if e.isWETH(tok.Address) && e.Prices != nil && e.Prices.Pool() != nil {
		hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		p, err := e.Prices.Pool().History(hctx, 1000, step)
		if err == nil {
			prices, src = p, "Uniswap v3 pool oracle (onchain)"
		}
	}
	if len(prices) < 60 && e.Tape != nil {
		pts := e.Tape.Range(sym, time.Now().Add(-6*time.Hour), time.Now(), 0)
		if len(pts) >= 60 {
			prices = make([]float64, len(pts))
			for i, p := range pts {
				prices[i] = p.P
			}
			src = "arena price record"
		}
	}
	if len(prices) < 60 {
		return nil, "", errors.New("not enough price history yet for this coin; try again later")
	}
	bt.mu.Lock()
	bt.prices[sym], bt.at[sym] = prices, time.Now()
	bt.mu.Unlock()
	return prices, src, nil
}

// orderCost is Flash's cost of one order of usd on sym: the average of a real
// buy quote and the matching sell quote. Cached for two minutes per size.
func (e *Engine) orderCost(ctx context.Context, tokAddr string, usd float64) (float64, string) {
	key := strings.ToLower(tokAddr) + "|" + strconv.FormatFloat(usd, 'f', 2, 64)
	bt.mu.Lock()
	if c, ok := bt.cost[key]; ok && time.Since(bt.costAt[key]) < 2*time.Minute {
		bt.mu.Unlock()
		return c, "Flash quotes (cached)"
	}
	bt.mu.Unlock()
	const fallback = 0.035 // typical fixed cost per order seen on Base, Nov 2026
	if e.Flash == nil {
		return fallback, "estimate"
	}
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req := func(side, qty string) *flash.QuoteRequest {
		return &flash.QuoteRequest{TargetChain: e.Cfg.Chain, ContraChain: e.Cfg.Chain, TargetAsset: tokAddr, ContraAsset: e.Cfg.USDC,
			Side: side, Qty: qty, OrderType: "market", MaxSlippage: e.Cfg.MaxSlippage, FlashIntegratorFeeBps: e.Cfg.FeeBps}
	}
	b, err := e.Flash.Quote(qctx, req("buy", strconv.FormatFloat(usd, 'f', 2, 64)))
	if err != nil || b.From == nil || b.To == nil || b.To.Amount.Float() <= 0 {
		return fallback, "estimate"
	}
	buy := b.From.Notional.Float() - b.To.Notional.Float()
	s, err := e.Flash.Quote(qctx, req("sell", strconv.FormatFloat(b.To.Amount.Float(), 'f', 12, 64)))
	c := buy
	if err == nil && s.From != nil && s.To != nil {
		c = (buy + s.From.Notional.Float() - s.To.Notional.Float()) / 2
	}
	if c < 0 {
		c = 0
	}
	bt.mu.Lock()
	bt.cost[key], bt.costAt[key] = c, time.Now()
	bt.mu.Unlock()
	return c, "Flash quotes at this size"
}

// Backtest runs agent rules over real past prices with real Flash costs.
func (e *Engine) Backtest(ctx context.Context, p arena.StrategyParams, sym string, sizeUSD float64, l store.AgentLimits) (BacktestReport, error) {
	strat, _, err := arena.BuildStrategy(p)
	if err != nil {
		return BacktestReport{}, err
	}
	if sym == "" {
		sym = e.Cfg.TradeAsset
	}
	tok, ok := e.tokenBySymbol(sym)
	if !ok {
		return BacktestReport{}, fmt.Errorf("unknown coin %s", sym)
	}
	if sizeUSD <= 0 {
		sizeUSD = 5
	}
	prices, src, err := e.history(ctx, tok.Symbol)
	if err != nil {
		return BacktestReport{}, err
	}
	if l.MaxTradeUSD > 0 && sizeUSD > l.MaxTradeUSD {
		sizeUSD = l.MaxTradeUSD
	}
	if l.MinTradeUSD > 0 && sizeUSD < l.MinTradeUSD {
		sizeUSD = l.MinTradeUSD
	}
	cost, costSrc := e.orderCost(ctx, tok.Address, sizeUSD)
	every := max(1, int(e.Cfg.AgentEvery/max(e.Cfg.PriceEvery, time.Second)))
	net := e.Cfg.NetTargets
	if l.NetTargets != "" {
		net = l.NetTargets == "on"
	}
	limit := e.Cfg.MaxEntryCostPct
	if l.MaxCostPct > 0 {
		limit = l.MaxCostPct
	}
	costPct := cost / sizeUSD * 100
	cool := l.CooldownMin * 60 / max(1, int(max(e.Cfg.PriceEvery, time.Second)/time.Second))
	var res arena.BacktestResult
	if limit > 0 && costPct > limit {
		res = arena.Backtest(strat, prices[:1], arena.BacktestOptions{SizeUSD: sizeUSD}) // no trades: every entry is over the cost limit
		res.Points, res.HoldPct = len(prices), (prices[len(prices)-1]/prices[0]-1)*100
	} else {
		res = arena.Backtest(strat, prices, arena.BacktestOptions{SizeUSD: sizeUSD, CostPerOrder: cost, Every: every, NetTargets: net, Cooldown: cool})
	}
	step := int(max(e.Cfg.PriceEvery, time.Second) / time.Second)
	return BacktestReport{BacktestResult: res, Token: tok.Symbol, Prices: prices, StepSec: step,
		StartAt: time.Now().Add(-time.Duration(len(prices)*step) * time.Second).UTC(), Hours: float64(len(prices)*step) / 3600,
		PriceSource: src, SizeUSD: sizeUSD, CostPerOrderUSD: cost, CostSource: costSrc,
		CostPct: math.Round(costPct*100) / 100, CostLimitPct: limit, CostBlocked: limit > 0 && costPct > limit}, nil
}

// RunTape records every coin's price each price check (for charts and
// backtests) and saves the record once a minute. On start, an empty or stale
// WETH record is filled from the pool's onchain oracle, so trades that were
// open before a restart still get a full chart.
func (e *Engine) RunTape(ctx context.Context) {
	if e.Tape == nil {
		return
	}
	every := max(e.Cfg.PriceEvery, time.Second)
	if e.Prices != nil && e.Prices.Pool() != nil {
		last := e.Tape.Range(e.Cfg.TradeAsset, time.Now().Add(-2*every), time.Now(), 0)
		// Fill the gap before recording anything new (the record only takes
		// points in time order). The pool must pass its startup check first,
		// so retry for up to a minute.
		for try := 0; len(last) == 0 && try < 12; try++ {
			hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			p, err := e.Prices.Pool().History(hctx, 1000, every)
			cancel()
			if err == nil {
				start := time.Now().Add(-time.Duration(len(p)) * every)
				for i, px := range p {
					e.Tape.Add(e.Cfg.TradeAsset, start.Add(time.Duration(i+1)*every), px)
				}
				e.Log.Info("price record filled from the pool oracle", "points", len(p))
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	save := time.NewTicker(time.Minute)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = e.Tape.Save()
			return
		case <-save.C:
			if err := e.Tape.Save(); err != nil {
				e.Log.Warn("price record save failed", "err", err)
			}
		case <-t.C:
			now := time.Now()
			if p, fresh := e.Prices.Snapshot(3 * every); fresh && len(p) > 0 {
				e.Tape.Add(e.Cfg.TradeAsset, now, p[len(p)-1])
			}
			for _, f := range e.TokenFeeds {
				if p, fresh := f.Snapshot(3 * every); fresh && len(p) > 0 {
					e.Tape.Add(f.Symbol(), now, p[len(p)-1])
				}
			}
		}
	}
}

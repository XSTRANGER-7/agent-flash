package arena

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/metrics"
)

// PriceFeed keeps a bounded WETH price history shared by every agent (one
// upstream read, N readers).
//
// Price: the onchain Uniswap v3 WETH/USDC pool on Base when a pool is set
// (independent of Flash, so it can check Flash quotes), else Flash /search.
// Exit-liquidity and risk flag: Flash /search when this key is eligible
// (some keys get 403 SEARCH_NOT_ELIGIBLE until they have Flash volume),
// otherwise the pool's onchain TVL.
type PriceFeed struct {
	fc      *flash.Client
	pool    *Pool
	symbol  string
	address string
	chain   string
	log     *slog.Logger

	mu             sync.RWMutex
	hist           []float64
	at             time.Time
	liquidity      float64
	liqSource      string
	risk           bool
	riskChecked    bool
	searchEligible *bool
	searchAt       time.Time
	tvlAt          time.Time
	poolErr        string
}

// NewPriceFeed: pool may be nil (Flash /search price only).
func NewPriceFeed(fc *flash.Client, pool *Pool, symbol, address, chain string, log *slog.Logger) *PriceFeed {
	return &PriceFeed{fc: fc, pool: pool, symbol: symbol, address: address, chain: chain, log: log}
}

func (p *PriceFeed) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh fetches one price now (used before the background loop warms up).
func (p *PriceFeed) Refresh(ctx context.Context) { p.tick(ctx) }

// Meta returns exit liquidity (USD), the risk flag, and whether both are known.
func (p *PriceFeed) Meta() (liquidity float64, riskFlagged bool, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.liquidity, p.risk, !p.at.IsZero() && p.liquidity > 0
}

// Status is what /v1/flash/status and /v1/price report.
type Status struct {
	Source          string    `json:"source"` // uniswap-v3 | flash-search
	Pool            string    `json:"pool,omitempty"`
	PoolError       string    `json:"poolError,omitempty"`
	LiquidityUSD    float64   `json:"liquidityUsd"`
	LiquiditySource string    `json:"liquiditySource"` // flash-search | pool-tvl
	RiskChecked     bool      `json:"riskChecked"`     // false = Flash risk flag not available for this key
	SearchEligible  *bool     `json:"searchEligible"`  // nil = not tried yet
	LastPriceAt     time.Time `json:"lastPriceAt"`
}

func (p *PriceFeed) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := Status{Source: "flash-search", LiquidityUSD: p.liquidity, LiquiditySource: p.liqSource, RiskChecked: p.riskChecked, SearchEligible: p.searchEligible, LastPriceAt: p.at, PoolError: p.poolErr}
	if p.pool != nil {
		s.Source, s.Pool = "uniswap-v3", p.pool.Address
	}
	return s
}

func (p *PriceFeed) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if p.pool == nil {
		p.tickSearch(cctx, true)
		return
	}
	if !p.pool.verified {
		if err := p.pool.Verify(cctx); err != nil {
			p.mu.Lock()
			p.poolErr = err.Error()
			p.mu.Unlock()
			p.log.Error("price pool verification failed; no price until fixed", "pool", p.pool.Address, "err", err)
			metrics.Inc("price_errors_total")
			return
		}
		p.mu.Lock()
		p.poolErr = ""
		p.mu.Unlock()
		p.log.Info("price pool verified (Uniswap v3 WETH/USDC)", "pool", p.pool.Address)
	}
	px, err := p.pool.Price(cctx)
	if err != nil {
		p.log.Warn("price feed (pool)", "err", err)
		metrics.Inc("price_errors_total")
		return
	}
	p.push(px)
	// Liquidity: the pool TVL first (the fallback must not starve), then Flash
	// /search every 15 min (it may become eligible later; on success it takes
	// over). Each call gets its own timeout: a slow DNS lookup for Flash used to
	// eat the shared one and leave liquidity at $0.
	tvl := func() {
		p.mu.RLock()
		need := p.liqSource != "flash-search" && time.Since(p.tvlAt) > 5*time.Minute
		p.mu.RUnlock()
		if !need {
			return
		}
		tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
		v, err := p.pool.TVL(tctx, px)
		tcancel()
		if err != nil {
			p.log.Warn("pool TVL", "err", err)
			return
		}
		p.mu.Lock()
		if p.liqSource != "flash-search" {
			p.liquidity, p.liqSource = v, "pool-tvl"
		}
		p.tvlAt = time.Now()
		p.mu.Unlock()
	}
	tvl()
	p.mu.RLock()
	trySearch := time.Since(p.searchAt) > 15*time.Minute
	p.mu.RUnlock()
	if trySearch {
		sctx, scancel := context.WithTimeout(ctx, 12*time.Second)
		p.tickSearch(sctx, false)
		scancel()
		tvl() // search just stopped working: the pool TVL takes over now
	}
}

// tickSearch reads Flash /search. usePrice=true when search is the price source.
func (p *PriceFeed) tickSearch(ctx context.Context, usePrice bool) {
	p.mu.Lock()
	p.searchAt = time.Now()
	p.mu.Unlock()
	assets, err := p.fc.Search(ctx, p.address, p.chain, 1)
	if err != nil {
		if flash.IsCode(err, flash.CodeSearchNotEligible) {
			f := false
			p.mu.Lock()
			first := p.searchEligible == nil || *p.searchEligible
			p.searchEligible = &f
			if p.liqSource == "flash-search" {
				p.liqSource = "" // stale; the pool TVL takes over
				p.tvlAt = time.Time{}
			}
			p.mu.Unlock()
			if first {
				p.log.Warn("Flash /search is not enabled for this API key (SEARCH_NOT_ELIGIBLE); using onchain pool TVL for liquidity. Risk flag unavailable.")
			}
			return
		}
		p.log.Warn("flash search", "err", err)
		metrics.Inc("price_errors_total")
		return
	}
	if len(assets) == 0 || !strings.EqualFold(assets[0].Address, p.address) {
		p.log.Warn("flash search: unexpected result", "n", len(assets))
		return
	}
	a := assets[0]
	t := true
	p.mu.Lock()
	p.searchEligible = &t
	p.risk, p.riskChecked = a.RiskFlagged, true
	if liq := a.Liquidity.Float(); liq > 0 { // "0" means unknown
		p.liquidity, p.liqSource = liq, "flash-search"
	}
	p.mu.Unlock()
	if usePrice {
		if px := a.Price.Float(); px > 0 {
			p.push(px)
		}
	}
}

func (p *PriceFeed) push(px float64) {
	p.mu.Lock()
	p.hist = append(p.hist, px)
	if len(p.hist) > 500 {
		p.hist = p.hist[len(p.hist)-500:]
	}
	p.at = time.Now()
	p.mu.Unlock()
	metrics.Set("price_usd", px)
}

// Snapshot returns a copy of the history and whether it is fresh.
func (p *PriceFeed) Snapshot(maxAge time.Duration) ([]float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	fresh := !p.at.IsZero() && time.Since(p.at) < maxAge
	return append([]float64(nil), p.hist...), fresh
}

func (p *PriceFeed) Last() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.hist) == 0 {
		return 0
	}
	return p.hist[len(p.hist)-1]
}

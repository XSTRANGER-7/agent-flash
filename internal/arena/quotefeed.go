package arena

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/flash"
)

// QuoteFeed prices a token from Flash itself: every tick it asks for a real
// $25 market quote (no funder, nothing signed) and reads Flash's valuation
// (to.notional / to.amount). Liquidity and the risk flag come from Flash
// /search when this key is eligible. Used for coins that have no pinned
// onchain pool (cbBTC today); WETH keeps the Uniswap pool feed.
type QuoteFeed struct {
	fc                    *flash.Client
	symbol, address, usdc string
	chain, feeBps         string
	log                   *slog.Logger

	mu        sync.Mutex
	hist      []float64
	at        time.Time
	liq       float64
	liqSource string
	risk      bool
	metaAt    time.Time
	lastErr   string
}

func NewQuoteFeed(fc *flash.Client, symbol, address, usdc, chain, feeBps string, log *slog.Logger) *QuoteFeed {
	return &QuoteFeed{fc: fc, symbol: symbol, address: address, usdc: usdc, chain: chain, feeBps: feeBps, log: log}
}

func (q *QuoteFeed) Symbol() string  { return q.symbol }
func (q *QuoteFeed) Address() string { return q.address }

func (q *QuoteFeed) Run(ctx context.Context, every time.Duration) {
	q.Refresh(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.Refresh(ctx)
		}
	}
}

// Refresh takes one price (and the search meta every 15 minutes).
func (q *QuoteFeed) Refresh(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	no := false
	res, err := q.fc.Quote(cctx, &flash.QuoteRequest{TargetChain: q.chain, ContraChain: q.chain, TargetAsset: q.address, ContraAsset: q.usdc,
		Side: "buy", Qty: "25", OrderType: "market", MaxSlippage: "0.03", FlashIntegratorFeeBps: q.feeBps, EVMUsePermit2: &no, ForceMinimalAllowance: true})
	q.mu.Lock()
	defer q.mu.Unlock()
	if err != nil || res.To == nil || res.To.Amount.Float() <= 0 || res.To.Notional.Float() <= 0 {
		if err != nil {
			q.lastErr = err.Error()
		}
		return
	}
	q.lastErr = ""
	q.hist = append(q.hist, res.To.Notional.Float()/res.To.Amount.Float())
	if len(q.hist) > 500 {
		q.hist = q.hist[len(q.hist)-500:]
	}
	q.at = time.Now()
	if time.Since(q.metaAt) > 15*time.Minute {
		q.metaAt = time.Now()
		q.mu.Unlock()
		assets, serr := q.fc.Search(cctx, q.address, q.chain, 1)
		q.mu.Lock()
		found := false
		if serr == nil {
			for _, a := range assets {
				if strings.EqualFold(a.Address, q.address) {
					q.risk = a.RiskFlagged
					if l := a.Liquidity.Float(); l > 0 {
						q.liq, q.liqSource, found = l, "flash-search", true
					}
				}
			}
		}
		if !found {
			// /search is not available to every key. Ask Flash what selling
			// $10k of the coin would do instead: if the estimated price impact
			// is small, an exit of our size is clearly possible. Liquidity is
			// then estimated as 10k / impact (impact 1% -> $1M).
			px := q.hist[len(q.hist)-1]
			q.mu.Unlock()
			probe, perr := q.fc.Quote(cctx, &flash.QuoteRequest{TargetChain: q.chain, ContraChain: q.chain, TargetAsset: q.address, ContraAsset: q.usdc,
				Side: "sell", Qty: strconv.FormatFloat(10000/px, 'f', 8, 64), OrderType: "market", MaxSlippage: "0.03",
				FlashIntegratorFeeBps: q.feeBps, EVMUsePermit2: &no, ForceMinimalAllowance: true})
			q.mu.Lock()
			if perr == nil && probe.To != nil && probe.To.Amount.Float() > 0 {
				impact := math.Max(probe.EstimatedPriceImpact.Float(), 1e-5)
				q.liq, q.liqSource = math.Min(10000/impact, 1e10), "flash-quote-impact"
			} else {
				q.metaAt = time.Now().Add(-14 * time.Minute) // retry in a minute
			}
		}
	}
}

func (q *QuoteFeed) Snapshot(maxAge time.Duration) ([]float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]float64(nil), q.hist...), len(q.hist) > 0 && time.Since(q.at) < maxAge
}

func (q *QuoteFeed) Last() float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.hist) == 0 {
		return 0
	}
	return q.hist[len(q.hist)-1]
}

// Meta is (liquidity USD, risk flag, known).
func (q *QuoteFeed) Meta() (float64, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.liq, q.risk, q.liq > 0
}

func (q *QuoteFeed) Err() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastErr
}

// LiquiditySource says where Meta's liquidity came from.
func (q *QuoteFeed) LiquiditySource() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.liqSource
}

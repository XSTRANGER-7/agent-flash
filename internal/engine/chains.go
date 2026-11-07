package engine

// Chains Flash executes on: live market data for each one (home page
// ticker) and a person's balances there (trade from your own wallet).
//
// Every number is read, never made up:
//   - price: a real Flash quote, $25 USDC -> the chain's gas coin, valued at
//     Flash's mark (to.notional / to.amount, before costs)
//   - cost: that quote's all-in cost (from.notional - to.notional)
//   - gas and block: eth_gasPrice / eth_blockNumber from the chain's RPC
//   - 24h change: ETH from the onchain price tape; other coins from our own
//     hourly samples, shown only once a sample from ~24 h ago exists

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/atomicfile"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// NativeAsset is how Flash names a chain's gas coin in a quote.
const NativeAsset = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// ChainStat is one chain in the ticker.
type ChainStat struct {
	Key       string    `json:"key"`
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Native    string    `json:"native"`
	Explorer  string    `json:"explorer"`
	Tradeable bool      `json:"tradeable"`
	Price     float64   `json:"price,omitempty"`     // gas coin in USD (Flash mark)
	Change24h *float64  `json:"change24h,omitempty"` // % vs ~24 h ago
	CostPct   float64   `json:"costPct,omitempty"`   // Flash all-in cost of a $25 buy
	GasGwei   float64   `json:"gasGwei,omitempty"`
	Block     uint64    `json:"block,omitempty"`
	PriceAt   time.Time `json:"priceAt,omitzero"`
	ChainAt   time.Time `json:"chainAt,omitzero"`
}

type chainBook struct {
	mu    sync.Mutex
	stats map[string]*ChainStat
	hist  map[string][]arena.TapePoint // hourly gas-coin prices (non-ETH coins)
	rpcs  map[string]*evm.RPC
	path  string
	dirty bool
}

var (
	booksMu sync.Mutex
	books   = map[*Engine]*chainBook{}
)

func (e *Engine) book() *chainBook {
	booksMu.Lock()
	defer booksMu.Unlock()
	b := books[e]
	if b == nil {
		b = &chainBook{stats: map[string]*ChainStat{}, hist: map[string][]arena.TapePoint{}, rpcs: map[string]*evm.RPC{}}
		if e.Cfg.DataDir != "" {
			b.path = filepath.Join(e.Cfg.DataDir, "chains.json")
			if data, _, err := atomicfile.Read(b.path); err == nil {
				_ = json.Unmarshal(data, &b.hist)
			}
		}
		for _, ch := range e.Cfg.Chains {
			b.stats[ch.Key] = &ChainStat{Key: ch.Key, ID: ch.ID, Name: ch.Name, Native: ch.Native, Explorer: ch.Explorer, Tradeable: ch.Tradeable()}
		}
		books[e] = b
	}
	return b
}

// ChainRPC is a JSON-RPC client for a chain (one per chain, reused).
func (e *Engine) ChainRPC(ch config.Chain) *evm.RPC {
	b := e.book()
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.rpcs[ch.Key]
	if r == nil {
		r = evm.NewRPC(ch.RPC)
		b.rpcs[ch.Key] = r
	}
	return r
}

// Chains is the ticker: every shown chain in the configured order.
func (e *Engine) Chains() []ChainStat {
	b := e.book()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ChainStat, 0, len(e.Cfg.Chains))
	for _, ch := range e.Cfg.Chains {
		if s := b.stats[ch.Key]; s != nil {
			c := *s
			if c.Change24h != nil {
				v := *c.Change24h
				c.Change24h = &v
			}
			out = append(out, c)
		}
	}
	return out
}

// RunChains refreshes the ticker every `every` (at least 30 s).
func (e *Engine) RunChains(ctx context.Context, every time.Duration) {
	every = max(every, 30*time.Second)
	e.refreshChains(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.saveChains()
			return
		case <-t.C:
			e.refreshChains(ctx)
		}
	}
}

func (e *Engine) refreshChains(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range e.Cfg.Chains {
		wg.Add(1)
		go func(ch config.Chain) { // chain reads in parallel (separate RPCs)
			defer wg.Done()
			e.readChain(ctx, ch)
		}(ch)
	}
	// Flash quotes one at a time: they share the API's rate limit with trading.
	for _, ch := range e.Cfg.Chains {
		if ctx.Err() != nil {
			break
		}
		e.quoteChain(ctx, ch)
	}
	wg.Wait()
	e.saveChains()
}

func (e *Engine) readChain(ctx context.Context, ch config.Chain) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r := e.ChainRPC(ch)
	gas, err := r.GasPrice(ctx)
	if err != nil {
		e.Log.Debug("chain gas read failed", "chain", ch.Key, "err", err)
		return
	}
	blk, err := r.BlockNumber(ctx)
	if err != nil {
		return
	}
	gwei, _ := new(big.Float).Quo(new(big.Float).SetInt(gas), big.NewFloat(1e9)).Float64()
	b := e.book()
	b.mu.Lock()
	if s := b.stats[ch.Key]; s != nil {
		s.GasGwei, s.Block, s.ChainAt = gwei, blk, time.Now().UTC()
	}
	b.mu.Unlock()
}

// quoteChain prices the chain's gas coin with a real Flash quote. No funder
// is sent, so Flash returns pricing only (nothing to sign).
func (e *Engine) quoteChain(ctx context.Context, ch config.Chain) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q, err := e.Flash.Quote(ctx, &flash.QuoteRequest{TargetChain: ch.Key, ContraChain: ch.Key, TargetAsset: NativeAsset, ContraAsset: ch.USDC.Address,
		Side: "buy", Qty: "25", OrderType: "market", FlashIntegratorFeeBps: e.Cfg.FeeBps})
	if err != nil {
		e.Log.Debug("chain price quote failed", "chain", ch.Key, "err", err)
		return
	}
	mark, _, _, pct, ok := quoteCosts(q, true)
	if !ok || mark <= 0 {
		return
	}
	now := time.Now().UTC()
	var chg *float64
	if ch.NativeIsETH() {
		chg = e.ethChange24h(now)
	}
	b := e.book()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !ch.NativeIsETH() {
		chg = b.sample(ch.Native, now, mark)
	}
	if s := b.stats[ch.Key]; s != nil {
		s.Price, s.CostPct, s.PriceAt, s.Change24h = mark, math.Round(pct*100)/100, now, chg
	}
}

// sample keeps one price an hour per coin (50 h) and returns the change
// against the sample nearest to 24 h ago, if there is one within 30 min.
func (b *chainBook) sample(coin string, now time.Time, px float64) *float64 {
	h := b.hist[coin]
	if n := len(h); n == 0 || now.UnixMilli()-h[n-1].T >= int64(time.Hour/time.Millisecond) {
		h = append(h, arena.TapePoint{T: now.UnixMilli(), P: px})
		cut := now.Add(-50 * time.Hour).UnixMilli()
		i := sort.Search(len(h), func(i int) bool { return h[i].T >= cut })
		h = h[i:]
		b.hist[coin] = h
		b.dirty = true
	}
	return changeVs(h, now, px)
}

func changeVs(h []arena.TapePoint, now time.Time, px float64) *float64 {
	want := now.Add(-24 * time.Hour).UnixMilli()
	best, gap := 0.0, int64(30*time.Minute/time.Millisecond)+1
	for _, p := range h {
		if d := abs64(p.T - want); d < gap && p.P > 0 {
			best, gap = p.P, d
		}
	}
	if best <= 0 {
		return nil
	}
	v := math.Round((px/best-1)*10000) / 100
	return &v
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// ethChange24h: ETH vs ~24 h ago from the onchain price tape (Base pool).
func (e *Engine) ethChange24h(now time.Time) *float64 {
	if e.Tape == nil {
		return nil
	}
	sym := e.Cfg.TradeAsset
	then := e.Tape.Range(sym, now.Add(-24*time.Hour-30*time.Minute), now.Add(-24*time.Hour+30*time.Minute), 0)
	last := e.Tape.Range(sym, now.Add(-10*time.Minute), now, 0)
	if len(then) == 0 || len(last) == 0 {
		return nil
	}
	return changeVs(then, now, last[len(last)-1].P)
}

func (e *Engine) saveChains() {
	b := e.book()
	b.mu.Lock()
	if !b.dirty || b.path == "" {
		b.mu.Unlock()
		return
	}
	data, err := json.Marshal(b.hist)
	b.dirty = false
	b.mu.Unlock()
	if err == nil {
		if err := atomicfile.Write(b.path, data, 0o644); err != nil {
			e.Log.Warn("save chain prices failed", "err", err)
		}
	}
}

// ChainBalance is what a wallet holds on one chain (read onchain).
type ChainBalance struct {
	Chain  string  `json:"chain"`
	USDC   float64 `json:"usdc"`
	ETH    float64 `json:"eth"`    // the chain's ETH token (WETH, WETH.e, ETH on BNB)
	Native float64 `json:"native"` // gas coin, for approvals
	Error  string  `json:"error,omitempty"`
}

// ChainBalances reads USDC, the ETH token and the gas coin of addr on ch.
func (e *Engine) ChainBalances(ctx context.Context, ch config.Chain, addr string) (ChainBalance, error) {
	out := ChainBalance{Chain: ch.Key}
	r := e.ChainRPC(ch)
	u, err := erc20Balance(ctx, r, ch.USDC.Address, addr)
	if err != nil {
		return out, err
	}
	out.USDC = decUnits(u, ch.USDC.Decimals)
	if ch.Tradeable() {
		if t, err := erc20Balance(ctx, r, ch.ETH.Address, addr); err == nil {
			out.ETH = decUnits(t, ch.ETH.Decimals)
		}
	}
	if n, err := r.ETHBalance(ctx, addr); err == nil {
		out.Native = decUnits(n, 18)
	}
	return out, nil
}

func erc20Balance(ctx context.Context, r *evm.RPC, token, owner string) (*big.Int, error) {
	a, err := evm.HexToAddressBytes(owner)
	if err != nil {
		return nil, err
	}
	data := append(append([]byte{}, balanceOfSelector...), make([]byte, 12)...)
	out, err := r.Call(ctx, token, append(data, a...))
	if err != nil {
		return nil, err
	}
	if len(out) < 32 {
		return nil, errors.New("balanceOf: short answer")
	}
	return new(big.Int).SetBytes(out[:32]), nil
}

func decUnits(n *big.Int, dec int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(n), new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))).Float64()
	return f
}

// TradeChain is the chain a trade was placed on ("" = home).
func (e *Engine) tradeChain(key string) config.Chain {
	if key == "" {
		return e.Cfg.Home()
	}
	if ch, ok := e.Cfg.ChainByKey(strings.ToLower(key)); ok {
		return ch
	}
	return e.Cfg.Home()
}

// tradeBalances: USDC and the bought token on the trade's own chain.
func (e *Engine) tradeBalances(ctx context.Context, w *wallet.Wallet, tr store.Trade, maxAge time.Duration) (float64, float64, error) {
	if tr.Chain == "" || strings.EqualFold(tr.Chain, e.Cfg.Chain) {
		usdc, _, err := e.Balances(ctx, w, maxAge)
		if err != nil {
			return 0, 0, err
		}
		tok, _ := e.TokenBalance(ctx, w, tr.TokenAddr, maxAge)
		return usdc, tok, nil
	}
	cb, err := e.ChainBalances(ctx, e.tradeChain(tr.Chain), w.Address())
	return cb.USDC, cb.ETH, err
}

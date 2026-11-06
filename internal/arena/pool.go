package arena

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/evm"
)

// Pool reads a Uniswap v3 pool onchain (eth_call only, nothing is signed).
// It is the independent reference price: the Flash quote is checked against it.
type Pool struct {
	Address string
	rpc     *evm.RPC

	// Set once by Verify (price-feed goroutine), read by charts and
	// backtests from other goroutines: guarded by mu.
	mu           sync.RWMutex
	wethIsToken0 bool
	weth, usdc   string
	decWETH      int
	decUSDC      int
	verified     bool
}

// params is a consistent copy of what Verify found.
func (p *Pool) params() (ok, wethIsToken0 bool, decWETH, decUSDC int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.verified, p.wethIsToken0, p.decWETH, p.decUSDC
}

var (
	selToken0    = []byte{0x0d, 0xfe, 0x16, 0x81} // token0()
	selToken1    = []byte{0xd2, 0x12, 0x20, 0xa7} // token1()
	selSlot0     = []byte{0x38, 0x50, 0xc7, 0xbd} // slot0()
	selDecimals  = []byte{0x31, 0x3c, 0xe5, 0x67} // decimals()
	selBalanceOf = []byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
)

func NewPool(rpc *evm.RPC, address, weth, usdc string) *Pool {
	return &Pool{Address: address, rpc: rpc, weth: weth, usdc: usdc}
}

func word(b []byte, i int) ([]byte, error) {
	if len(b) < (i+1)*32 {
		return nil, errors.New("short eth_call result")
	}
	return b[i*32 : (i+1)*32], nil
}

func wordAddr(b []byte) (string, error) {
	w, err := word(b, 0)
	if err != nil {
		return "", err
	}
	return strings.ToLower(evm.ChecksumAddress(w[12:])), nil
}

// Verify checks that the pool really pairs WETH with USDC and reads decimals.
func (p *Pool) Verify(ctx context.Context) error {
	r0, err := p.rpc.Call(ctx, p.Address, selToken0)
	if err != nil {
		return fmt.Errorf("pool token0: %w", err)
	}
	r1, err := p.rpc.Call(ctx, p.Address, selToken1)
	if err != nil {
		return fmt.Errorf("pool token1: %w", err)
	}
	t0, err := wordAddr(r0)
	if err != nil {
		return err
	}
	t1, err := wordAddr(r1)
	if err != nil {
		return err
	}
	weth, usdc := strings.ToLower(p.weth), strings.ToLower(p.usdc)
	var w0 bool
	switch {
	case t0 == weth && t1 == usdc:
		w0 = true
	case t0 == usdc && t1 == weth:
		w0 = false
	default:
		return fmt.Errorf("pool %s pairs %s/%s, not WETH/USDC", p.Address, t0, t1)
	}
	dW, err := p.decimals(ctx, p.weth)
	if err != nil {
		return err
	}
	dU, err := p.decimals(ctx, p.usdc)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.wethIsToken0, p.decWETH, p.decUSDC, p.verified = w0, dW, dU, true
	p.mu.Unlock()
	return nil
}

func (p *Pool) decimals(ctx context.Context, token string) (int, error) {
	r, err := p.rpc.Call(ctx, token, selDecimals)
	if err != nil {
		return 0, fmt.Errorf("decimals(%s): %w", token, err)
	}
	w, err := word(r, 0)
	if err != nil {
		return 0, err
	}
	d := new(big.Int).SetBytes(w).Int64()
	if d <= 0 || d > 36 {
		return 0, fmt.Errorf("decimals(%s) = %d", token, d)
	}
	return int(d), nil
}

// Price returns the USD (USDC) price of one WETH from slot0.
func (p *Pool) Price(ctx context.Context) (float64, error) {
	ok, w0, dW, dU := p.params()
	if !ok {
		return 0, errors.New("pool not verified")
	}
	r, err := p.rpc.Call(ctx, p.Address, selSlot0)
	if err != nil {
		return 0, fmt.Errorf("slot0: %w", err)
	}
	w, err := word(r, 0)
	if err != nil {
		return 0, err
	}
	return PriceFromSqrtX96(new(big.Int).SetBytes(w), w0, dW, dU)
}

// PriceFromSqrtX96 converts a Uniswap v3 sqrtPriceX96 into USDC per WETH.
// raw1/raw0 = (sqrtPriceX96 / 2^96)^2.
func PriceFromSqrtX96(sqrtX96 *big.Int, wethIsToken0 bool, decWETH, decUSDC int) (float64, error) {
	if sqrtX96.Sign() <= 0 {
		return 0, errors.New("pool has no price")
	}
	sq := new(big.Float).SetPrec(256).SetInt(sqrtX96)
	ratio := new(big.Float).SetPrec(256).Mul(sq, sq)
	ratio.Quo(ratio, new(big.Float).SetPrec(256).SetMantExp(big.NewFloat(1), 192)) // raw token1 per raw token0
	r, _ := ratio.Float64()
	var px float64
	if wethIsToken0 { // token1 = USDC
		px = r * math.Pow10(decWETH-decUSDC)
	} else { // token0 = USDC, token1 = WETH
		if r == 0 {
			return 0, errors.New("pool has no price")
		}
		px = 1 / (r * math.Pow10(decUSDC-decWETH))
	}
	if px <= 0 || math.IsInf(px, 0) || math.IsNaN(px) {
		return 0, errors.New("pool price out of range")
	}
	return px, nil
}

var selObserve = []byte{0x88, 0x3b, 0xdb, 0xfd} // observe(uint32[])

// pad32 encodes a non-negative number as one 32-byte ABI word.
func pad32(v int64) []byte {
	b := make([]byte, 32)
	new(big.Int).SetInt64(v).FillBytes(b)
	return b
}

// History reads past prices from the pool's own oracle (observe): n average
// prices, one per step, oldest first, ending now. The pool only remembers a
// few hours (its observation buffer); when n*step reaches past that, it asks
// for fewer points. Each price is the average over its step (time-weighted,
// so one odd trade inside a block does not move it).
func (p *Pool) History(ctx context.Context, n int, step time.Duration) ([]float64, error) {
	ok, w0, dW, dU := p.params()
	if !ok {
		return nil, errors.New("pool not verified")
	}
	sec := int64(step / time.Second)
	if sec <= 0 || n < 2 {
		return nil, errors.New("history: bad step or size")
	}
	var lastErr error
	for ; n >= 20; n = n * 3 / 4 {
		data := append([]byte{}, selObserve...)
		data = append(data, pad32(32)...)
		data = append(data, pad32(int64(n+1))...)
		for i := n; i >= 0; i-- {
			data = append(data, pad32(int64(i)*sec)...)
		}
		r, err := p.rpc.Call(ctx, p.Address, data)
		if err != nil {
			lastErr = err // "OLD": asked further back than the pool remembers
			continue
		}
		off, err := word(r, 0)
		if err != nil {
			return nil, err
		}
		o := int(new(big.Int).SetBytes(off).Int64())
		if o+32 > len(r) || int(new(big.Int).SetBytes(r[o:o+32]).Int64()) != n+1 || o+32*(n+2) > len(r) {
			return nil, errors.New("history: malformed observe result")
		}
		ticks := make([]*big.Int, n+1)
		for i := 0; i <= n; i++ {
			w := r[o+32*(i+1) : o+32*(i+2)]
			v := new(big.Int).SetBytes(w)
			if w[0]&0x80 != 0 { // int56 sign-extended to 256 bits
				v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
			}
			ticks[i] = v
		}
		out := make([]float64, 0, n)
		for i := 0; i < n; i++ {
			d := new(big.Int).Sub(ticks[i+1], ticks[i])
			tick, _ := new(big.Float).Quo(new(big.Float).SetInt(d), big.NewFloat(float64(sec))).Float64()
			px := math.Pow(1.0001, tick) // raw token1 per raw token0
			if w0 {
				px *= math.Pow10(dW - dU)
			} else {
				px = 1 / (px * math.Pow10(dU-dW))
			}
			if px <= 0 || math.IsInf(px, 0) || math.IsNaN(px) {
				return nil, errors.New("history: price out of range")
			}
			out = append(out, px)
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("history: too few points")
	}
	return nil, fmt.Errorf("pool history: %w", lastErr)
}

// TVL is the USD value held by the pool (USDC + WETH at price).
func (p *Pool) TVL(ctx context.Context, price float64) (float64, error) {
	bal := func(token string, dec int) (float64, error) {
		addr, err := evm.HexToAddressBytes(p.Address)
		if err != nil {
			return 0, err
		}
		data := append(append([]byte{}, selBalanceOf...), make([]byte, 12)...)
		data = append(data, addr...)
		r, err := p.rpc.Call(ctx, token, data)
		if err != nil {
			return 0, err
		}
		w, err := word(r, 0)
		if err != nil {
			return 0, err
		}
		f, _ := new(big.Float).Quo(new(big.Float).SetInt(new(big.Int).SetBytes(w)), new(big.Float).SetFloat64(math.Pow10(dec))).Float64()
		return f, nil
	}
	_, _, dW, dU := p.params()
	u, err := bal(p.usdc, dU)
	if err != nil {
		return 0, err
	}
	w, err := bal(p.weth, dW)
	if err != nil {
		return 0, err
	}
	return u + w*price, nil
}

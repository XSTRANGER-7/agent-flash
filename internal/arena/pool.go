package arena

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/vikram/agent-arena/internal/evm"
)

// Pool reads a Uniswap v3 pool onchain (eth_call only, nothing is signed).
// It is the independent reference price: the Flash quote is checked against it.
type Pool struct {
	Address string
	rpc     *evm.RPC

	wethIsToken0 bool
	weth, usdc   string
	decWETH      int
	decUSDC      int
	verified     bool
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
	switch {
	case t0 == weth && t1 == usdc:
		p.wethIsToken0 = true
	case t0 == usdc && t1 == weth:
		p.wethIsToken0 = false
	default:
		return fmt.Errorf("pool %s pairs %s/%s, not WETH/USDC", p.Address, t0, t1)
	}
	if p.decWETH, err = p.decimals(ctx, p.weth); err != nil {
		return err
	}
	if p.decUSDC, err = p.decimals(ctx, p.usdc); err != nil {
		return err
	}
	p.verified = true
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
	if !p.verified {
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
	return PriceFromSqrtX96(new(big.Int).SetBytes(w), p.wethIsToken0, p.decWETH, p.decUSDC)
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
	u, err := bal(p.usdc, p.decUSDC)
	if err != nil {
		return 0, err
	}
	w, err := bal(p.weth, p.decWETH)
	if err != nil {
		return 0, err
	}
	return u + w*price, nil
}

package arena

import (
	"math"
	"math/big"
	"testing"
)

// sqrtX96 builds sqrtPriceX96 for a given raw token1/token0 ratio.
func sqrtX96(ratio float64) *big.Int {
	s := new(big.Float).SetPrec(256).SetFloat64(math.Sqrt(ratio))
	s.Mul(s, new(big.Float).SetPrec(256).SetMantExp(big.NewFloat(1), 96))
	i, _ := s.Int(nil)
	return i
}

func TestPriceFromSqrtX96(t *testing.T) {
	// token0 = WETH (18), token1 = USDC (6): raw ratio = 2236.51 * 1e6 / 1e18.
	px, err := PriceFromSqrtX96(sqrtX96(2236.51*1e-12), true, 18, 6)
	if err != nil || math.Abs(px-2236.51) > 0.01 {
		t.Fatalf("weth token0: %v %v", px, err)
	}
	// Reversed pool: token0 = USDC, token1 = WETH: raw ratio = 1e18 / (2236.51 * 1e6).
	px, err = PriceFromSqrtX96(sqrtX96(1e12/2236.51), false, 18, 6)
	if err != nil || math.Abs(px-2236.51) > 0.01 {
		t.Fatalf("weth token1: %v %v", px, err)
	}
	if _, err := PriceFromSqrtX96(big.NewInt(0), true, 18, 6); err == nil {
		t.Fatal("zero price must error")
	}
}

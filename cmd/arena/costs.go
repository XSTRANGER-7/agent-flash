package main

// `arena costs`: what a round trip really costs on Flash right now, per size.
// Read-only: it asks Flash for quotes (buy USDC->WETH, then sell that WETH
// back) and prints the all-in cost. Nothing is signed or sent.
//
// Flash quotes are all-in (gas is inside), so the cost has a fixed part per
// order: small trades pay a bigger share. Use this to pick the smallest size
// whose cost still fits MAX_ENTRY_COST_PCT.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/flash"
)

func costs(args []string) error {
	fs := flag.NewFlagSet("costs", flag.ExitOnError)
	sizes := fs.String("sizes", "1,2,3,4,5,8", "trade sizes in USD, comma-separated")
	_ = fs.Parse(args)
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	var list []float64
	for _, s := range strings.Split(*sizes, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || v <= 0 || v > 1000 {
			return fmt.Errorf("bad size %q", s)
		}
		list = append(list, v)
	}
	fc := flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fmt.Printf("Real Flash quotes now (WETH on Base, integrator fee %s bps). Nothing is signed or sent.\n", cfg.FeeBps)
	fmt.Printf("  %-7s %-16s %-16s %s\n", "size", "buy cost", "sell cost", "round trip")
	best, bestPct := 0.0, 1e9
	for _, usd := range list {
		buy, sell, err := roundTrip(ctx, cfg, fc, usd, cfg.FeeBps)
		if err != nil {
			fmt.Printf("  $%-6.2f %s\n", usd, shortRPC(err))
			continue
		}
		rt := buy + sell
		fmt.Printf("  $%-6.2f $%.3f (%5.2f%%)  $%.3f (%5.2f%%)  $%.3f (%5.2f%%)\n", usd, buy, buy/usd*100, sell, sell/usd*100, rt, rt/usd*100)
		if buy/usd*100 <= cfg.MaxEntryCostPct && rt/usd < bestPct {
			best, bestPct = usd, rt/usd
		}
	}
	fmt.Printf("Entry cost guard: MAX_ENTRY_COST_PCT=%.1f%% (agents); test trades allow %.0f%%.\n", cfg.MaxEntryCostPct, cfg.TestMaxCostPct)
	if best > 0 {
		fmt.Printf("Cheapest size per dollar that passes the guard: $%.2f (round trip %.2f%%).\n", best, bestPct*100)
	}
	return nil
}

// roundTrip returns the dollar cost of buying `usd` of WETH and selling it back.
func roundTrip(ctx context.Context, cfg *config.Config, fc *flash.Client, usd float64, feeBps string) (buyCost, sellCost float64, err error) {
	qctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	b, err := fc.Quote(qctx, &flash.QuoteRequest{TargetChain: cfg.Chain, ContraChain: cfg.Chain, TargetAsset: cfg.WETH, ContraAsset: cfg.USDC,
		Side: "buy", Qty: strconv.FormatFloat(usd, 'f', 2, 64), OrderType: "market", MaxSlippage: cfg.MaxSlippage, FlashIntegratorFeeBps: feeBps})
	if err != nil {
		return 0, 0, err
	}
	if b.From == nil || b.To == nil || b.To.Amount.Float() <= 0 {
		return 0, 0, errors.New("buy quote without amounts")
	}
	buyCost = b.From.Notional.Float() - b.To.Notional.Float()
	s, err := fc.Quote(qctx, &flash.QuoteRequest{TargetChain: cfg.Chain, ContraChain: cfg.Chain, TargetAsset: cfg.WETH, ContraAsset: cfg.USDC,
		Side: "sell", Qty: strconv.FormatFloat(b.To.Amount.Float(), 'f', 12, 64), OrderType: "market", MaxSlippage: cfg.MaxSlippage, FlashIntegratorFeeBps: feeBps})
	if err != nil {
		return buyCost, 0, err
	}
	if s.From == nil || s.To == nil {
		return buyCost, 0, errors.New("sell quote without amounts")
	}
	return buyCost, s.From.Notional.Float() - s.To.Notional.Float(), nil
}

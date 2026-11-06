package main

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/wallet"
)

// retry runs fn up to n times (2s, 4s backoff), each try with its own 20s budget.
func retry(ctx context.Context, n int, fn func(context.Context) error) error {
	var err error
	for i := 0; i < n; i++ {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = fn(c)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if i < n-1 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(2<<i) * time.Second):
			}
		}
	}
	return err
}

type walletLine struct {
	w        *wallet.Wallet
	usdc     float64
	weth     float64
	eth      float64
	err      error
	isAgent  bool
	isFunded bool
}

// doctorWallets reads wallet balances in parallel (each with its own
// timeout). Balances are information, not a requirement to start, so a slow
// read is a warning. Agents and funded wallets get a line each; the rest of
// a long list is summarised.
func doctorWallets(ctx context.Context, cfg *config.Config, fc *flash.Client, rpc *evm.RPC, list []*wallet.Wallet, tank bool,
	ok, warn func(name, detail string)) {
	lines := make([]walletLine, len(list))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, w := range list {
		wg.Add(1)
		go func(i int, w *wallet.Wallet) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			l := walletLine{w: w, isAgent: w.Role == wallet.RoleAgent}
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			bals, err := fc.Balances(c, w.Address())
			if err != nil {
				l.err = err
			} else {
				l.usdc, l.weth = flash.BalanceOf(bals, cfg.Chain, cfg.USDC), flash.BalanceOf(bals, cfg.Chain, cfg.WETH)
			}
			if b, err := rpc.ETHBalance(c, w.Address()); err == nil {
				l.eth, _ = new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e18)).Float64()
			}
			l.isFunded = l.usdc > 0 || l.weth > 0 || l.eth > 0
			lines[i] = l
		}(i, w)
	}
	wg.Wait()
	sort.SliceStable(lines, func(a, b int) bool { return lines[a].isAgent && !lines[b].isAgent })
	quiet, unread := 0, 0
	var testWallets []string
	for _, l := range lines {
		id := l.w.ID
		if strings.HasPrefix(id, "e2e-") || strings.HasPrefix(id, "plan-e2e-") {
			testWallets = append(testWallets, id)
		}
		if !l.isAgent && !l.isFunded && len(lines) > 12 {
			if l.err != nil {
				unread++
			} else {
				quiet++
			}
			continue // summarised below
		}
		if l.err != nil {
			warn("wallet "+id, fmt.Sprintf("%s  balance not read: %v", l.w.Address(), l.err))
			continue
		}
		line := fmt.Sprintf("%s  USDC %.2f  WETH %.6f  ETH %.5f", l.w.Address(), l.usdc, l.weth, l.eth)
		maxTrade := math.Min(cfg.MaxTradeUSD, l.usdc*cfg.MaxPositionPct/100)
		switch {
		case cfg.Paper:
			ok("wallet "+id, line+"  (paper mode trades mock USDC)")
		case !l.isFunded:
			warn("wallet "+id, line+"  (not funded)")
		case l.eth < 0.00005 && !tank:
			warn("wallet "+id, line+"  (needs a little ETH for approvals, or run `arena gastank`)")
		case maxTrade < 1:
			warn("wallet "+id, line+fmt.Sprintf("  (largest trade $%.2f)", maxTrade))
		default:
			ok("wallet "+id, line)
		}
	}
	if quiet+unread > 0 {
		msg := fmt.Sprintf("%d more empty wallets", quiet+unread)
		if unread > 0 {
			msg += fmt.Sprintf(" (%d not read in time)", unread)
		}
		ok("other wallets", msg)
	}
	if len(testWallets) > 0 {
		warn("test wallets", fmt.Sprintf("%d wallets left by e2e runs; remove the empty ones with: arena prune-test-wallets", len(testWallets)))
	}
}

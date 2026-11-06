package main

// `arena gastank`: create (once) and show the gas tank. The tank is a wallet
// that holds a little ETH on Base and pays gas so people only hold USDC:
// approvals and withdrawals of the arena's sub-wallets, and gasless USDC
// deposits (EIP-3009) from people's own wallets. Flash already pays the gas
// of every order it executes.

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/wallet"
)

func gasTankCmd() error {
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	addr, created, err := wallet.EnsureGasTank(cfg.WalletsFile)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("Gas tank created (its key is in", cfg.WalletsFile+", section \"gas\"). Back the file up.")
		fmt.Println("Restart the server so it uses the tank.")
	}
	fmt.Println("Gas tank:", addr)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if b, err := evm.NewRPC(cfg.RPCURL).ETHBalance(ctx, addr); err == nil {
		eth, _ := new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e18)).Float64()
		fmt.Printf("ETH on Base: %.6f\n", eth)
	}
	fmt.Printf("Fund it with a little ETH on the Base network (about $1 covers hundreds of approvals).\n")
	fmt.Printf("Limits: %.6f ETH a day (GAS_TANK_DAILY_ETH), %.6f ETH per top-up (GAS_DRIP_ETH), %d top-ups per wallet a day.\n",
		cfg.GasDailyETH, cfg.GasDripETH, cfg.GasDripsPerWalletDay)
	return nil
}

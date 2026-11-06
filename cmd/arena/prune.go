package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// pruneTestWallets removes the sub-wallets end-to-end test runs created
// (ids starting with e2e- or plan-e2e-), but only those that hold nothing
// onchain (ETH, USDC, WETH and every configured coin all zero). Their keys
// go to a dated backup file, never deleted.
func pruneTestWallets(args []string) error {
	fs := flag.NewFlagSet("prune-test-wallets", flag.ExitOnError)
	dry := fs.Bool("dry", false, "only list what would be removed")
	_ = fs.Parse(args)
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("arena serve is running on %s; stop it first", addr)
	}
	ws, err := wallet.Load(cfg.WalletsFile)
	if err != nil {
		return err
	}
	rpc := evm.NewRPC(cfg.RPCURL)
	tokens := []string{cfg.USDC}
	for _, t := range cfg.Tokens {
		tokens = append(tokens, t.Address)
	}
	var remove []string
	kept := 0
	for _, w := range ws.List(wallet.RoleFollower) {
		if !strings.HasPrefix(w.ID, "e2e-") && !strings.HasPrefix(w.ID, "plan-e2e-") {
			continue
		}
		empty, why := true, ""
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := retry(ctx, 3, func(c context.Context) error {
			b, e := rpc.ETHBalance(c, w.Address())
			if e != nil {
				return e
			}
			if b.Sign() > 0 {
				empty, why = false, "holds ETH"
				return nil
			}
			for _, t := range tokens {
				out, e := rpc.Call(c, t, append(sel("70a08231"), pad(addr20(w.Address()))...))
				if e != nil {
					return e
				}
				if new(big.Int).SetBytes(out).Sign() > 0 {
					empty, why = false, "holds tokens"
					return nil
				}
			}
			return nil
		})
		cancel()
		if err != nil {
			empty, why = false, "balance unreadable ("+err.Error()+")"
		}
		if !empty {
			fmt.Printf("  keep    %-34s %s\n", w.ID, why)
			kept++
			continue
		}
		fmt.Printf("  remove  %-34s %s (empty onchain)\n", w.ID, w.Address())
		remove = append(remove, w.ID)
	}
	if len(remove) == 0 {
		fmt.Println("no empty test wallets to remove")
		return nil
	}
	if *dry {
		fmt.Printf("dry run: %d would be removed, %d kept\n", len(remove), kept)
		return nil
	}
	backup := cfg.WalletsFile + ".removed-" + time.Now().Format("20060102-150405")
	n, err := wallet.RemoveFromFile(cfg.WalletsFile, remove, backup)
	if err != nil {
		return err
	}
	// Stop their follows and plans so nothing keeps trying to trade them.
	base := envOr("DATA_DIR", "./data")
	for _, dir := range []string{base, filepath.Join(base, "paper")} {
		st, err := store.Open(dir)
		if err != nil {
			continue
		}
		for _, id := range remove {
			for _, f := range st.FollowsOf(id) {
				st.Unfollow(id, f.AgentID)
			}
		}
		for _, p := range st.Plans() {
			for _, id := range remove {
				if p.WalletID == id && (p.Status == "active" || p.Status == "paused") {
					st.UpdatePlan(p.ID, func(x *store.Plan) { x.Status, x.StatusReason = "stopped", "its test wallet was removed" })
				}
			}
		}
		_ = st.Save()
	}
	fmt.Printf("removed %d empty test wallets (keys kept in %s), kept %d\n", n, backup, kept)
	return nil
}

func addr20(a string) []byte { b, _ := evm.HexToAddressBytes(a); return b }

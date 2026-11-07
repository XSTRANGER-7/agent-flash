// Command arena runs the Agent Arena backend.
//
//	arena keygen  -agents trendhunter,dipbuyer,contrarian -followers follower1
//	arena doctor                       # read-only check of key, chain, price, stream, wallets
//	arena wallets                      # addresses + balances
//	arena price                        # onchain price + Flash quote mark
//	arena smoke   -wallet trendhunter -usd 5   # one bracket trade (respects DRY_RUN)
//	arena serve                        # agents + copy engine + API
//
// Settings come from the environment; a .env file in the working directory
// (or ENV_FILE) is loaded first without overriding variables already set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vikram/agent-arena/internal/approval"
	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/httpapi"
	"github.com/vikram/agent-arena/internal/outbox"
	"github.com/vikram/agent-arena/internal/paper"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	envFile := envOr("ENV_FILE", ".env")
	if err := config.LoadDotEnv(envFile); err != nil {
		fmt.Fprintln(os.Stderr, "error: reading", envFile+":", err)
		os.Exit(1)
	}
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "doctor":
		err = doctor(log)
	case "wallets":
		err = withEngine(log, func(ctx context.Context, e *engine.Engine) error { return printWallets(ctx, e) })
	case "price":
		err = price(log)
	case "smoke":
		err = smoke(log, os.Args[2:])
	case "serve":
		err = serve(log)
	case "payout":
		err = payout(os.Args[2:])
	case "reset-paper":
		err = resetPaper()
	case "prune-test-wallets":
		err = pruneTestWallets(os.Args[2:])
	case "simulate":
		err = simulate()
	case "costs":
		err = costs(os.Args[2:])
	case "gastank":
		err = gasTankCmd()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "  costs [-sizes 1,2,5]              real Flash round-trip cost per trade size (quotes only)")
	fmt.Fprintln(os.Stderr, "  gastank                           create / show the gas tank that makes trading gasless for people")
	fmt.Fprintln(os.Stderr, "  simulate                          onchain checks on real Base state with test money (nothing broadcast)")
	fmt.Fprintln(os.Stderr, "  reset-paper                       start paper money over (old paper data is kept in a backup folder)")
	fmt.Fprintln(os.Stderr, "  prune-test-wallets [-dry]         remove EMPTY wallets left by e2e runs (keys kept in a backup file)")
	fmt.Fprintln(os.Stderr, "  payout   -wallet <id> -to 0x...   lock where a sub-wallet's withdrawals go (server stopped)")
	fmt.Fprintln(os.Stderr, "usage: arena <keygen|doctor|wallets|price|smoke|serve> [flags]")
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	agents := fs.String("agents", "trendhunter,dipbuyer,contrarian", "comma-separated agent wallet ids")
	followers := fs.String("followers", "follower1", "comma-separated follower wallet ids")
	out := fs.String("out", envOr("WALLETS_FILE", "./wallets.json"), "output file (chmod 600)")
	_ = fs.Parse(args)
	addrs, err := wallet.Generate(*out, split(*agents), split(*followers))
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (owner-only). Back it up: losing it loses the funds.\nFund each address on Base with USDC + ~$0.50 of ETH for approvals:\n", *out)
	for id, a := range addrs {
		fmt.Printf("  %-14s %s\n", id, a)
	}
	return nil
}

func newPriceFeed(cfg *config.Config, fc *flash.Client, rpc *evm.RPC, log *slog.Logger) *arena.PriceFeed {
	var pool *arena.Pool
	if cfg.PricePool != "" {
		pool = arena.NewPool(rpc, cfg.PricePool, cfg.WETH, cfg.USDC)
	}
	return arena.NewPriceFeed(fc, pool, cfg.TradeAsset, cfg.WETH, cfg.Chain, log)
}

// quoteMark asks Flash for a $10 market quote (no funder, nothing signed) and
// returns Flash's WETH valuation and the all-in cost.
func quoteMark(ctx context.Context, cfg *config.Config, fc *flash.Client) (mark, costPct float64, err error) {
	q, err := fc.Quote(ctx, &flash.QuoteRequest{TargetChain: cfg.Chain, ContraChain: cfg.Chain, TargetAsset: cfg.WETH, ContraAsset: cfg.USDC,
		Side: "buy", Qty: "10", OrderType: "market", FlashIntegratorFeeBps: cfg.FeeBps})
	if err != nil {
		return 0, 0, err
	}
	if q.To == nil || q.From == nil || q.To.Amount.Float() <= 0 || q.From.Notional.Float() <= 0 {
		return 0, 0, errors.New("quote without amounts")
	}
	return q.To.Notional.Float() / q.To.Amount.Float(), (q.From.Notional.Float() - q.To.Notional.Float()) / q.From.Notional.Float() * 100, nil
}

func price(log *slog.Logger) error {
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fc := flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, log)
	pf := newPriceFeed(cfg, fc, evm.NewRPC(cfg.RPCURL), log)
	pf.Refresh(ctx)
	st := pf.Status()
	if pf.Last() <= 0 {
		return fmt.Errorf("no price from %s (%s)", st.Source, st.PoolError)
	}
	fmt.Printf("WETH onchain (%s %s): $%.2f · liquidity $%.0f (%s)\n", st.Source, st.Pool, pf.Last(), st.LiquidityUSD, st.LiquiditySource)
	mark, cost, err := quoteMark(ctx, cfg, fc)
	if err != nil {
		return fmt.Errorf("flash quote (is FLASH_API_KEY valid?): %w", err)
	}
	fmt.Printf("WETH on Flash (quote mark): $%.2f · difference %.2f%% · all-in cost of a $10 buy %.2f%%\n", mark, (mark/pf.Last()-1)*100, cost)
	return nil
}

func doctor(log *slog.Logger) error {
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fails := 0
	ok := func(name, detail string) { fmt.Printf("  ok    %-22s %s\n", name, detail) }
	warn := func(name, detail string) { fmt.Printf("  warn  %-22s %s\n", name, detail) }
	bad := func(name string, err error) { fails++; fmt.Printf("  FAIL  %-22s %v\n", name, err) }

	fmt.Println("Agent Arena doctor (read-only: nothing is signed or sent)")
	switch cfg.ModeName() {
	case "paper":
		ok("mode", fmt.Sprintf("PAPER: mock money ($%.0f per wallet), real Flash quotes + signing, simulated fills", cfg.PaperUSDC))
	case "dry":
		ok("mode", "DRY RUN: orders are signed but never sent")
	default:
		auto := "agents and plans do NOT auto-trade (LIVE_AGENTS=false)"
		if cfg.LiveAgents {
			auto = "agents and plans auto-trade (LIVE_AGENTS=true)"
		}
		warn("mode", fmt.Sprintf("LIVE: real money, budget $%.2f; %s", cfg.LiveBudgetUSD, auto))
	}

	rpc := evm.NewRPC(cfg.RPCURL)
	// Networks blip (a DNS miss, a slow handshake): retry before calling it broken.
	var id int64
	err = retry(ctx, 3, func(c context.Context) (e error) { id, e = rpc.ChainID(c); return })
	if err != nil {
		bad("base rpc", fmt.Errorf("%w (tried 3 times; check your internet or RPC_URL)", err))
	} else if id != cfg.ChainID {
		bad("base rpc", fmt.Errorf("chain id %d, expected %d", id, cfg.ChainID))
	} else {
		ok("base rpc", fmt.Sprintf("%s (chain %d)", cfg.RPCURL, id))
	}

	fc := flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, quiet)
	pf := newPriceFeed(cfg, fc, rpc, quiet)
	pf.Refresh(ctx)
	st := pf.Status()
	if pf.Last() <= 0 {
		bad("reference price", fmt.Errorf("no price from %s: %s", st.Source, st.PoolError))
	} else {
		ok("reference price", fmt.Sprintf("$%.2f from %s %s", pf.Last(), st.Source, st.Pool))
		if st.LiquidityUSD >= cfg.MinLiquidityUSD {
			ok("exit liquidity", fmt.Sprintf("$%.0f (%s, minimum $%.0f)", st.LiquidityUSD, st.LiquiditySource, cfg.MinLiquidityUSD))
		} else {
			bad("exit liquidity", fmt.Errorf("$%.0f below MIN_LIQUIDITY_USD $%.0f", st.LiquidityUSD, cfg.MinLiquidityUSD))
		}
	}
	switch {
	case st.SearchEligible == nil:
		warn("flash /search", "no answer in time (slow network/DNS?); the server retries every 15 min and uses the pool TVL meanwhile")
	case *st.SearchEligible:
		ok("flash /search", "enabled for this key (liquidity + risk flag from Flash)")
	default:
		warn("flash /search", "SEARCH_NOT_ELIGIBLE for this key; using onchain pool TVL. Ask Definitive for access to get the risk flag.")
	}

	var mark, cost float64
	if err := retry(ctx, 3, func(c context.Context) (e error) { mark, cost, e = quoteMark(c, cfg, fc); return }); err != nil {
		bad("flash key + quote", err)
	} else {
		d := ""
		if pf.Last() > 0 {
			d = fmt.Sprintf(" (%.2f%% vs onchain)", (mark/pf.Last()-1)*100)
		}
		ok("flash key + quote", fmt.Sprintf("mark $%.2f%s · $10 buy costs %.2f%% all-in", mark, d, cost))
	}

	if !cfg.StreamEnabled {
		warn("flash stream", "disabled (FLASH_STREAM=false); polling only")
	} else if err := flash.PingStream(ctx, cfg.StreamURL, cfg.FlashAPIKey); err != nil {
		warn("flash stream", fmt.Sprintf("%v; polling will be used", err))
	} else {
		ok("flash stream", cfg.StreamURL+" (heartbeat received)")
	}

	if len(cfg.SettlementAllowlist) == 0 {
		warn("settlement pin", "FLASH_SETTLEMENT_ALLOWLIST empty: first contract seen is pinned (set 0x5d00000873b6BF41539e6f5365B0Ff7d3c368f78)")
	} else {
		ok("settlement pin", strings.Join(cfg.SettlementAllowlist, ","))
	}
	if len(cfg.VaultAllowlist) == 0 {
		warn("vault pin", "FLASH_VAULT_ALLOWLIST empty: first vault seen is pinned")
	} else {
		ok("vault pin", strings.Join(cfg.VaultAllowlist, ","))
	}

	ws, err := wallet.Load(cfg.WalletsFile)
	if err != nil {
		bad("wallets", err)
	} else {
		t, hasTank := ws.GasTank()
		doctorWallets(ctx, cfg, fc, rpc, ws.List(""), hasTank, ok, warn)
		switch {
		case !hasTank:
			warn("gas tank", "none: every wallet pays its own gas. `arena gastank` makes people's trading gasless")
		default:
			if b, err := rpc.ETHBalance(ctx, t.Address()); err != nil {
				warn("gas tank", t.Address()+"  balance not read: "+err.Error())
			} else if eth, _ := new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e18)).Float64(); eth < 0.0002 && !cfg.Paper {
				warn("gas tank", fmt.Sprintf("%s  ETH %.6f: low, send ~$1 of ETH on Base", t.Address(), eth))
			} else {
				ok("gas tank", fmt.Sprintf("%s  ETH %.6f (pays approvals, withdrawals and gasless deposits)", t.Address(), eth))
			}
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	fmt.Println("All required checks passed.")
	return nil
}

func smoke(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	wid := fs.String("wallet", "trendhunter", "wallet id")
	usd := fs.Float64("usd", 5, "USDC to spend")
	tp := fs.Float64("tp", 3, "take profit % above price (before net-of-cost adjustment)")
	sl := fs.Float64("sl", 2, "stop loss % below price")
	test := fs.Bool("test", false, "test trade: TEST_TRADE_USD, TEST_TP_PCT / TEST_SL_PCT, test cost cap, no net-target bump")
	_ = fs.Parse(args)
	return withEngine(log, func(ctx context.Context, e *engine.Engine) error {
		w, ok := e.Wallets.Get(*wid)
		if !ok {
			return fmt.Errorf("wallet %s not found", *wid)
		}
		e.Prices.Refresh(ctx)
		px := e.Prices.Last()
		if px <= 0 {
			return fmt.Errorf("no reference price (%s)", e.Prices.Status().PoolError)
		}
		fmt.Printf("wallet %s %s | WETH $%.2f (%s) | mode=%s\n", w.ID, w.Address(), px, e.Prices.Status().Source, e.Cfg.ModeName())
		var opts engine.BuyOpts
		if *test {
			opts = engine.BuyOpts{MaxCostPct: e.Cfg.TestMaxCostPct, NoNetTargets: true}
			*usd, *tp, *sl = e.Cfg.TestTradeUSD, e.Cfg.TestTPPct, e.Cfg.TestSLPct
		}
		if d := e.LiveBudgetCheck(*usd); d != "" {
			return errors.New(d)
		}
		p, err := e.Exec.Buy(ctx, w, *usd, round2(px*(1+*tp/100)), round2(px*(1-*sl/100)), 0, px, opts)
		if err != nil {
			return err
		}
		fmt.Printf("placed: orderId=%s quoteId=%s approveTx=%s mark=$%.2f allIn=$%.2f cost=$%.3f (%.2f%%) TP=%.2f SL=%.2f\n",
			p.OrderID, p.QuoteID, p.ApproveTx, p.MarkPrice, p.QuotedPrice, p.CostUSD, p.CostPct, p.TakeProfit, p.StopLoss)
		for _, s := range p.Steps {
			fmt.Printf("  %s  %-12s %s\n", s.At.Format("15:04:05"), s.Step, s.Detail)
		}
		if !p.DryRun {
			fmt.Println("track it: https://basescan.org/address/" + w.Address())
		}
		return nil
	})
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func printWallets(ctx context.Context, e *engine.Engine) error {
	rpc := evm.NewRPC(e.Cfg.RPCURL)
	for _, w := range e.Wallets.List("") {
		u, we, err := e.Balances(ctx, w, 0)
		if err != nil {
			fmt.Printf("%-14s %-9s %s  (balance error: %v)\n", w.ID, w.Role, w.Address(), err)
			continue
		}
		eth := 0.0
		if b, err := rpc.ETHBalance(ctx, w.Address()); err == nil {
			eth, _ = new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e18)).Float64()
		}
		fmt.Printf("%-14s %-9s %s  USDC %.2f  WETH %.6f  ETH %.5f\n", w.ID, w.Role, w.Address(), u, we, eth)
	}
	return nil
}

func loadCfg() (*config.Config, error) {
	// ADMIN_TOKEN is only required for `serve`.
	if os.Getenv("ADMIN_TOKEN") == "" && os.Args[1] != "serve" {
		os.Setenv("ADMIN_TOKEN", "cli-only-not-used-0000")
	}
	return config.Load()
}

// withEngine builds the full dependency graph.
func withEngine(log *slog.Logger, fn func(context.Context, *engine.Engine) error) error {
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	ws, err := wallet.Load(cfg.WalletsFile)
	if err != nil {
		return err
	}
	// New accounts, sub-wallets and plans add keys to the wallets file. Say so
	// at start when it cannot be written (e.g. a read-only container mount).
	if f, err := os.OpenFile(cfg.WalletsFile, os.O_WRONLY, 0); err != nil {
		log.Warn("wallets file is not writable: new accounts, sub-wallets and plans cannot be created", "file", cfg.WalletsFile, "err", err)
	} else {
		f.Close()
	}
	if cfg.Paper {
		// Paper results never mix with real ones.
		cfg.DataDir = filepath.Join(cfg.DataDir, "paper")
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	for id, addr := range st.SelfWallets() { // people's own wallets (addresses only, no keys)
		if _, err := ws.WatchID(id, addr); err != nil {
			log.Warn("own wallet not restored", "id", id, "err", err)
		}
	}
	ob, err := outbox.Open(filepath.Join(cfg.DataDir, "outbox"), cfg.ArkivSyncURL, cfg.ArkivSyncToken, log)
	if err != nil {
		return err
	}
	defer ob.Close()
	fc := flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, log)
	rpc := evm.NewRPC(cfg.RPCURL)
	// The feed is saved: every post goes to DATA_DIR/feed.jsonl and the
	// newest 20,000 come back after a restart (the full history page).
	fd, err := feed.Open(filepath.Join(cfg.DataDir, "feed.jsonl"), 20000)
	if err != nil {
		return fmt.Errorf("feed history: %w", err)
	}
	defer fd.Close()
	roster := arena.DefaultRoster()
	allowed := map[string]bool{cfg.TradeAsset: true}
	for _, t := range cfg.Tokens {
		allowed[t.Symbol] = true
	}
	ref := referee.New(referee.Policy{
		AllowedTokens: allowed, MaxTradeUSD: cfg.MaxTradeUSD, MaxPositionPct: cfg.MaxPositionPct,
		MaxTradesPerDay: cfg.MaxTradesPerDay, MaxOpenPerAgent: 1, MinTPPct: 0.5, MaxSLPct: 5,
		MinLiquidityUSD: cfg.MinLiquidityUSD, MaxDrawdownPct: cfg.MaxAgentDrawdownPct,
	})
	apr := approval.NewManager(cfg.ApprovalTimeout, nil, func(r *approval.Request) {
		fd.Publish(feed.Post{Kind: "approval", AgentID: r.AgentID, Text: fmt.Sprintf("Approval %s for %s: %s ($%.2f)", r.Status, r.WalletID, r.Summary, r.AmountUSD), Data: map[string]any{"approvalId": r.ID, "status": r.Status}})
	})
	pf := newPriceFeed(cfg, fc, rpc, log)
	e := &engine.Engine{
		Cfg: cfg, Log: log, Flash: fc,
		Exec:    engine.NewExecutor(cfg, fc, rpc, log),
		Referee: ref, Approvals: apr, Store: st, Feed: fd, Outbox: ob, Wallets: ws,
		Prices: pf,
		Tape:   arena.NewTape(filepath.Join(cfg.DataDir, "prices.json")),
		Agents: roster,
	}
	if t, ok := ws.GasTank(); ok {
		e.Exec.SetGasTank(t)
		log.Info("gas tank on: people only need USDC", "address", t.Address(), "dailyEth", cfg.GasDailyETH)
	}
	e.TokenFeeds = map[string]*arena.QuoteFeed{}
	for _, t := range cfg.Tokens[1:] {
		e.TokenFeeds[strings.ToLower(t.Address)] = arena.NewQuoteFeed(fc, t.Symbol, t.Address, cfg.USDC, cfg.Chain, cfg.FeeBps, log)
	}
	if cfg.Paper {
		v, err := paper.New(paper.Options{BaseURL: cfg.FlashBaseURL, APIKey: cfg.FlashAPIKey, FeeBps: cfg.FeeBps, Chain: cfg.Chain,
			USDC: cfg.USDC, WETH: cfg.WETH, SeedUSDC: cfg.PaperUSDC, Price: e.TokenPrice, Dir: filepath.Join(cfg.DataDir, "venue"), Log: log})
		if err != nil {
			return err
		}
		fc.SetTransport(v) // quotes + search still reach the real Flash API through it
		if v.WasReset() {
			// Fresh paper balances: old equity would read as huge losses and
			// trip the circuit breaker. Start the scoreboard over too.
			st.ResetPerformance()
			log.Warn("paper ledger reset: equity history cleared so scores start fresh")
		}
		e.Venue = v
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e.Ctx = ctx
	defer func() {
		e.Wait()
		if err := st.Save(); err != nil {
			log.Error("final save failed", "err", err)
		}
	}()
	return fn(ctx, e)
}

func serve(log *slog.Logger) error {
	return withEngine(log, func(ctx context.Context, e *engine.Engine) error {
		log.Info("starting agent arena", "config", e.Cfg.Redacted())
		switch e.Cfg.ModeName() {
		case "dry":
			log.Warn("MODE=dry: orders are quoted and signed but NOT submitted.")
		case "paper":
			log.Warn("MODE=paper: mock balances, real Flash quotes and signing, simulated fills. No real money moves.", "seedUsdc", e.Cfg.PaperUSDC, "data", e.Cfg.DataDir)
			go e.Venue.Run(ctx, 3*time.Second)
		case "live":
			log.Warn("MODE=live: REAL orders with real money.", "liveAgents", e.Cfg.LiveAgents, "liveBudgetUsd", e.Cfg.LiveBudgetUSD, "spentUsd", e.LiveSpent())
		}
		for _, w := range e.Wallets.List("") {
			log.Info("wallet", "id", w.ID, "role", w.Role, "address", w.Address())
		}
		e.LoadUserAgents() // agents people made (they trade from their own accounts)
		e.RestoreOpen()
		e.PublishProfiles()

		if e.Cfg.StreamEnabled {
			noPermit2 := false
			e.Stream = &flash.Stream{URL: e.Cfg.StreamURL, Key: e.Cfg.FlashAPIKey, Log: log, OnOrders: e.OnStreamOrders,
				Quote: &flash.QuoteRequest{TargetChain: e.Cfg.Chain, ContraChain: e.Cfg.Chain, TargetAsset: e.Cfg.WETH, ContraAsset: e.Cfg.USDC,
					Side: "buy", Qty: strconv.FormatFloat(e.Cfg.MaxTradeUSD, 'f', 2, 64), OrderType: "market", MaxSlippage: e.Cfg.MaxSlippage,
					FlashIntegratorFeeBps: e.Cfg.FeeBps, EVMUsePermit2: &noPermit2}}
			go e.Stream.Run(ctx)
		}
		go e.Prices.Run(ctx, e.Cfg.PriceEvery)
		for _, t := range e.Cfg.Tokens[1:] {
			if err := verifyDecimals(ctx, e.Cfg.RPCURL, t.Address, t.Decimals); err != nil {
				log.Error("token disabled: onchain check failed", "token", t.Symbol, "err", err)
				delete(e.TokenFeeds, strings.ToLower(t.Address))
				continue
			}
			go e.TokenFeeds[strings.ToLower(t.Address)].Run(ctx, e.Cfg.PriceEvery)
		}
		go e.RunPlans(ctx)
		if bot := (&engine.Broadcaster{DiscordWebhook: e.Cfg.DiscordWebhook, TelegramToken: e.Cfg.TelegramToken, TelegramChat: e.Cfg.TelegramChat, PublicURL: e.Cfg.PublicURL}); bot.Enabled() {
			log.Info("signal bot on", "discord", e.Cfg.DiscordWebhook != "", "telegram", e.Cfg.TelegramToken != "")
			go e.RunBroadcast(ctx, bot)
		}
		go e.RunAgents(ctx)
		go e.WatchOrders(ctx)
		go e.SampleEquity(ctx)
		go e.RunTape(ctx)
		go e.RunRisk(ctx)
		go e.Outbox.Run(ctx)
		go func() { // periodic snapshot
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := e.Store.Save(); err != nil {
						log.Error("snapshot failed", "err", err)
					}
				}
			}
		}()

		srv := &http.Server{
			Addr: e.Cfg.HTTPAddr, Handler: httpapi.New(e, log).Handler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
		}
		errc := make(chan error, 1)
		go func() {
			log.Info("http listening", "addr", e.Cfg.HTTPAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		select {
		case <-ctx.Done():
		case err := <-errc:
			return err
		}
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	})
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// payout locks where a sub-wallet's withdrawals may go. It is a server-side
// command on purpose: the HTTP API can never change a payout address, so a
// stolen admin token cannot redirect anyone's money.
func payout(args []string) error {
	fs := flag.NewFlagSet("payout", flag.ExitOnError)
	wid := fs.String("wallet", "", "wallet id")
	to := fs.String("to", "", "owner's own 0x address on Base")
	_ = fs.Parse(args)
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`).MatchString(*to) {
		return errors.New("-to must be a 0x address")
	}
	ws, err := wallet.Load(cfg.WalletsFile)
	if err != nil {
		return err
	}
	w, ok := ws.Get(*wid)
	if !ok {
		return fmt.Errorf("wallet %q not found", *wid)
	}
	for _, o := range ws.List("") {
		if strings.EqualFold(o.Address(), *to) {
			return errors.New("payout cannot be one of the arena's own sub-wallets")
		}
	}
	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("arena serve is running on %s; stop it first (it would overwrite this change on its next save)", addr)
	}
	base := envOr("DATA_DIR", "./data")
	for _, dir := range []string{base, filepath.Join(base, "paper")} {
		st, err := store.Open(dir)
		if err != nil {
			return err
		}
		st.SetPayout(w.ID, *to)
		if err := st.Save(); err != nil {
			return err
		}
	}
	fmt.Printf("payout for %s (%s) locked to %s\n", w.ID, w.Address(), *to)
	return nil
}

// verifyDecimals reads decimals() onchain so a wrong EXTRA_TOKENS entry can
// never size an order 10^n off.
func verifyDecimals(ctx context.Context, rpcURL, token string, want int) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := evm.NewRPC(rpcURL).Call(cctx, token, []byte{0x31, 0x3c, 0xe5, 0x67})
	if err != nil {
		return err
	}
	if got := new(big.Int).SetBytes(out); got.Int64() != int64(want) {
		return fmt.Errorf("decimals() = %s, config says %d", got, want)
	}
	return nil
}

// resetPaper starts paper trading over: data/paper is moved to a dated
// backup (nothing is deleted). Real-money data in data/ is not touched.
func resetPaper() error {
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
	dir := filepath.Join(envOr("DATA_DIR", "./data"), "paper")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Println("no paper data yet; nothing to reset")
		return nil
	}
	bak := dir + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.Rename(dir, bak); err != nil {
		return err
	}
	fmt.Println("paper data moved to", bak, "- the next start begins with fresh paper money")
	return nil
}

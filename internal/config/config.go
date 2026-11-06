// Package config loads all runtime settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Flash
	FlashAPIKey  string
	FlashBaseURL string
	FeeBps       string // integrator fee, sent on quote AND order (same value)
	// Settlement contracts we are willing to sign for (EIP-712 verifyingContract,
	// also the approve spender). Empty = trust-on-first-use with a loud log line.
	SettlementAllowlist []string
	// Vaults the FlashOrder message may name (funds move through it).
	// Empty = trust-on-first-use, like the settlement contract.
	VaultAllowlist []string
	// Builder code sent as erc8021AttributionCode on POST /order (optional).
	AttributionCode string
	// Order WebSocket (wss://.../v1/ws). Polling stays on as a fallback.
	StreamEnabled bool
	StreamURL     string

	// Chain
	Chain      string // flash chain name, e.g. "base"
	ChainID    int64  // 8453 for Base mainnet
	RPCURL     string
	USDC       string
	WETH       string
	TradeAsset string // symbol label used in posts

	// Wallets (Level 1 sub-wallets: one EOA per agent / follower)
	WalletsFile string

	// Safety
	// Tokens that can be traded (agents trade TradeAsset = WETH; personal plans
	// may pick any of these). Symbol, address and decimals, verified onchain at startup.
	Tokens []Token

	// Mode: "paper" (mock balances, real Flash quotes + signing, simulated fills),
	// "dry" (quote + sign, never submit) or "live" (real orders). DryRun mirrors
	// Mode == "dry"; Paper mirrors Mode == "paper".
	Mode      string
	DryRun    bool
	Paper     bool
	PaperUSDC float64 // mock USDC each wallet starts with in paper mode
	// Live mode guards: agents only trade real money when LiveAgents is on;
	// LiveBudgetUSD caps the total USDC ever put into live entries.
	LiveAgents    bool
	LiveBudgetUSD float64
	// Test trades (operator "\ test"): own size, cost cap and bracket.
	TestTradeUSD         float64
	TestMaxCostPct       float64
	TestTPPct            float64
	TestSLPct            float64
	MaxApprovalUSD       float64 // ERC-20 approval cap per wallet (Lock 2)
	ApprovalThresholdUSD float64 // copy trades above this need a human
	ApprovalTimeout      time.Duration
	MaxTradeUSD          float64
	// MinTradeUSD raises a %-sized agent / copy trade to at least this much
	// when the wallet holds it. Flash's cost per order is mostly fixed, so on
	// a small wallet a 20% slice is too small to pass the cost guard. 0 = off.
	MinTradeUSD     float64
	MaxPositionPct  float64
	MaxTradesPerDay int
	// Competitive-safety guards (see doc tab 7)
	MaxPriceDeviationPct float64 // refuse quotes that drift too far from the reference price
	MinLiquidityUSD      float64 // refuse tokens that may be hard to sell ("can't sell" problem)
	MaxAgentDrawdownPct  float64 // circuit breaker: pause an agent (and its copiers) past this drawdown
	// Execution-cost guards (Flash quotes include a ~$0.06 fixed cost + ~0.36%).
	MaxSlippage     string  // sent as maxSlippage on quote + order ("0.03" = 3%)
	MaxEntryCostPct float64 // refuse entries whose all-in cost (from.notional - to.notional) is above this %
	NetTargets      bool    // raise take profit by the estimated round-trip cost so a "win" is a real win
	BreakEvenStop   bool    // once price is halfway from break-even to TP, move the stop to break-even (PATCH)

	// Reference price: Uniswap v3 WETH/USDC pool on Base (empty = Flash /search).
	PricePool string

	// Loops
	PriceEvery  time.Duration
	AgentEvery  time.Duration
	EquityEvery time.Duration
	OrderPoll   time.Duration

	// Copy engine
	CopyWorkers int

	// HTTP
	HTTPAddr    string
	AdminToken  string
	CORSOrigins []string

	// Signal bot (optional): agent trades and results to Discord / Telegram.
	DiscordWebhook string
	TelegramToken  string
	TelegramChat   string
	PublicURL      string // links in bot posts and share cards

	// Storage
	DataDir        string
	ArkivSyncURL   string // optional sidecar that publishes to Arkiv
	ArkivSyncToken string
	SeasonID       string
}

func Load() (*Config, error) {
	c := &Config{
		FlashAPIKey:          os.Getenv("FLASH_API_KEY"),
		FlashBaseURL:         env("FLASH_BASE_URL", "https://flash.definitive.fi/v1"),
		FeeBps:               env("FLASH_FEE_BPS", "10"),
		SettlementAllowlist:  list("FLASH_SETTLEMENT_ALLOWLIST"),
		VaultAllowlist:       list("FLASH_VAULT_ALLOWLIST"),
		AttributionCode:      strings.TrimSpace(os.Getenv("FLASH_ATTRIBUTION_CODE")),
		StreamEnabled:        boolean("FLASH_STREAM", true),
		StreamURL:            os.Getenv("FLASH_WS_URL"),
		MaxSlippage:          env("MAX_SLIPPAGE", "0.03"),
		MaxEntryCostPct:      num("MAX_ENTRY_COST_PCT", 2.5),
		NetTargets:           boolean("NET_TARGETS", true),
		BreakEvenStop:        boolean("BREAKEVEN_STOP", true),
		PricePool:            env("PRICE_POOL", "0xd0b53D9277642d899DF5C87A3966A349A798F224"),
		Chain:                env("CHAIN", "base"),
		ChainID:              int64(num("CHAIN_ID", 8453)),
		RPCURL:               env("RPC_URL", "https://mainnet.base.org"),
		USDC:                 env("USDC_ADDRESS", "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"),
		WETH:                 env("WETH_ADDRESS", "0x4200000000000000000000000000000000000006"),
		TradeAsset:           env("TRADE_SYMBOL", "WETH"),
		WalletsFile:          env("WALLETS_FILE", "./wallets.json"),
		Mode:                 strings.ToLower(strings.TrimSpace(os.Getenv("MODE"))),
		PaperUSDC:            num("PAPER_USDC", 100),
		LiveAgents:           boolean("LIVE_AGENTS", false),
		LiveBudgetUSD:        num("LIVE_BUDGET_USD", 5),
		TestTradeUSD:         num("TEST_TRADE_USD", 1),
		TestMaxCostPct:       num("TEST_MAX_COST_PCT", 12),
		TestTPPct:            num("TEST_TP_PCT", 2),
		TestSLPct:            num("TEST_SL_PCT", 2),
		MaxApprovalUSD:       num("MAX_APPROVAL_USD", 10),
		ApprovalThresholdUSD: num("APPROVAL_THRESHOLD_USD", 10),
		ApprovalTimeout:      dur("APPROVAL_TIMEOUT", 2*time.Minute),
		MaxTradeUSD:          num("MAX_TRADE_USD", 5),
		MinTradeUSD:          num("MIN_TRADE_USD", 0),
		MaxPositionPct:       num("MAX_POSITION_PCT", 25),
		MaxTradesPerDay:      int(num("MAX_TRADES_PER_DAY", 5)),
		MaxPriceDeviationPct: num("MAX_PRICE_DEVIATION_PCT", 1.5),
		MinLiquidityUSD:      num("MIN_LIQUIDITY_USD", 1_000_000),
		MaxAgentDrawdownPct:  num("MAX_AGENT_DRAWDOWN_PCT", 20),
		PriceEvery:           dur("PRICE_EVERY", 15*time.Second),
		AgentEvery:           dur("AGENT_EVERY", 30*time.Second),
		EquityEvery:          dur("EQUITY_EVERY", 60*time.Second),
		OrderPoll:            dur("ORDER_POLL", 5*time.Second),
		CopyWorkers:          int(num("COPY_WORKERS", 4)),
		HTTPAddr:             env("HTTP_ADDR", ":8080"),
		AdminToken:           os.Getenv("ADMIN_TOKEN"),
		CORSOrigins:          list("CORS_ORIGINS"),
		DataDir:              env("DATA_DIR", "./data"),
		DiscordWebhook:       strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL")),
		TelegramToken:        strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChat:         strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		PublicURL:            strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"),
		ArkivSyncURL:         os.Getenv("ARKIV_SYNC_URL"),
		ArkivSyncToken:       os.Getenv("ARKIV_SYNC_TOKEN"),
		SeasonID:             env("SEASON_ID", "s1"),
	}
	c.Tokens = []Token{{Symbol: c.TradeAsset, Address: c.WETH, Decimals: 18}}
	var errs []string
	specs := list("EXTRA_TOKENS")
	if _, set := os.LookupEnv("EXTRA_TOKENS"); !set {
		// Coinbase Wrapped BTC on Base (decimals are re-checked onchain at startup).
		specs = []string{"cbBTC:0xcbB7C0000aB88B473b1f5aFd9ef808440eed33Bf:8"}
	}
	for _, spec := range specs {
		p := strings.Split(spec, ":")
		if len(p) != 3 {
			errs = append(errs, "EXTRA_TOKENS entries must look like SYMBOL:0xADDRESS:DECIMALS")
			continue
		}
		d, err := strconv.Atoi(strings.TrimSpace(p[2]))
		if err != nil || d <= 0 || d > 18 || !isAddress(strings.TrimSpace(p[1])) {
			errs = append(errs, "EXTRA_TOKENS entries must look like SYMBOL:0xADDRESS:DECIMALS")
			continue
		}
		c.Tokens = append(c.Tokens, Token{Symbol: strings.TrimSpace(p[0]), Address: strings.TrimSpace(p[1]), Decimals: d})
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MODE")), "paper") {
		// Demo money: the $10 allowance cap and $10 approval threshold only
		// protect real funds, so paper mode lifts them (still configurable).
		c.MaxTradeUSD = num("PAPER_MAX_TRADE_USD", 100)
		c.MaxApprovalUSD = math.Max(c.MaxApprovalUSD, c.MaxTradeUSD)
		c.ApprovalThresholdUSD = num("PAPER_APPROVAL_THRESHOLD_USD", 50)
	}
	if c.FlashAPIKey == "" {
		errs = append(errs, "FLASH_API_KEY is required")
	} else if !strings.HasPrefix(c.FlashAPIKey, "dpka_") {
		errs = append(errs, "FLASH_API_KEY should start with dpka_")
	}
	if len(c.AdminToken) < 16 {
		errs = append(errs, "ADMIN_TOKEN must be at least 16 characters")
	}
	if _, err := strconv.Atoi(c.FeeBps); err != nil {
		errs = append(errs, "FLASH_FEE_BPS must be an integer")
	}
	if c.MaxApprovalUSD <= 0 || c.MaxTradeUSD <= 0 {
		errs = append(errs, "MAX_APPROVAL_USD and MAX_TRADE_USD must be > 0")
	}
	if c.MaxTradeUSD > c.MaxApprovalUSD {
		errs = append(errs, "MAX_TRADE_USD must not exceed MAX_APPROVAL_USD (the allowance cap would block the order)")
	}
	if s, err := strconv.ParseFloat(c.MaxSlippage, 64); err != nil || s <= 0 || s > 0.5 {
		errs = append(errs, "MAX_SLIPPAGE must be a decimal fraction in (0, 0.5], e.g. 0.03")
	}
	if c.AttributionCode != "" && !validAttribution(c.AttributionCode) {
		errs = append(errs, "FLASH_ATTRIBUTION_CODE must be 1-32 chars of [a-zA-Z0-9_.-]")
	}
	if c.ChainID != 8453 {
		errs = append(errs, "CHAIN_ID must be 8453 (Base mainnet)")
	}
	for _, a := range append(append([]string{}, c.SettlementAllowlist...), c.VaultAllowlist...) {
		if !isAddress(a) {
			errs = append(errs, "allowlist entry "+a+" is not a 0x address")
		}
	}
	if c.PricePool != "" && !isAddress(c.PricePool) {
		errs = append(errs, "PRICE_POOL must be a 0x address")
	}
	switch c.Mode {
	case "":
		// Backwards compatible: DRY_RUN alone picks dry or live.
		c.Mode = "dry"
		if !boolean("DRY_RUN", true) {
			c.Mode = "live"
		}
	case "dry", "paper", "live":
	default:
		errs = append(errs, "MODE must be paper, dry or live")
	}
	c.DryRun, c.Paper = c.Mode == "dry", c.Mode == "paper"
	if c.Mode == "live" && c.LiveBudgetUSD <= 0 {
		errs = append(errs, "LIVE_BUDGET_USD must be > 0 in live mode (it caps total real money put into trades)")
	}
	if c.TestTradeUSD <= 0 || c.TestTradeUSD > c.MaxTradeUSD {
		errs = append(errs, "TEST_TRADE_USD must be > 0 and <= MAX_TRADE_USD")
	}
	if c.MinTradeUSD < 0 || c.MinTradeUSD > c.MaxTradeUSD {
		errs = append(errs, "MIN_TRADE_USD must be 0 (off) or up to MAX_TRADE_USD")
	}
	if c.StreamURL == "" {
		c.StreamURL = streamURL(c.FlashBaseURL)
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	return c, nil
}

// Redacted is safe to log (no keys or tokens).
func (c *Config) Redacted() string {
	return fmt.Sprintf("mode=%s liveAgents=%v liveBudgetUSD=%.2f chain=%s chainId=%d dryRun=%v feeBps=%s maxTradeUSD=%.2f maxApprovalUSD=%.2f approvalThresholdUSD=%.2f maxSlippage=%s maxEntryCostPct=%.2f netTargets=%v pricePool=%s stream=%v settlementPinned=%d vaultPinned=%d http=%s arkivSync=%v",
		c.Mode, c.LiveAgents, c.LiveBudgetUSD, c.Chain, c.ChainID, c.DryRun, c.FeeBps, c.MaxTradeUSD, c.MaxApprovalUSD, c.ApprovalThresholdUSD, c.MaxSlippage, c.MaxEntryCostPct, c.NetTargets, c.PricePool, c.StreamEnabled, len(c.SettlementAllowlist), len(c.VaultAllowlist), c.HTTPAddr, c.ArkivSyncURL != "")
}

// LoadDotEnv reads KEY=VALUE lines from path into the environment without
// overriding variables that are already set. A missing file is not an error.
// Values are never logged.
func LoadDotEnv(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 { // trailing comment on an unquoted value
			v = strings.TrimSpace(v[:i])
		}
		if _, set := os.LookupEnv(k); !set && k != "" {
			os.Setenv(k, v)
		}
	}
	return nil
}

func isAddress(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, r := range s[2:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func validAttribution(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// streamURL turns https://host/v1 into wss://host/v1/ws.
func streamURL(base string) string {
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + "/ws"
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://") + "/ws"
	}
	return ""
}

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func num(k string, d float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return d
}

func boolean(k string, d bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return d
}

func dur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if x, err := time.ParseDuration(v); err == nil {
			return x
		}
	}
	return d
}

func list(k string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(k), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Live is true when orders go to Flash with real money.
func (c *Config) Live() bool { return !c.DryRun && !c.Paper }

// ModeName is Mode with the empty value (tests, old configs) resolved.
func (c *Config) ModeName() string {
	switch {
	case c.Paper:
		return "paper"
	case c.DryRun:
		return "dry"
	}
	return "live"
}

// Token is one tradeable ERC-20 on Base.
type Token struct {
	Symbol   string `json:"symbol"`
	Address  string `json:"address"`
	Decimals int    `json:"decimals"`
}

// TokenBySymbol finds a tradeable token (case-insensitive).
func (c *Config) TokenBySymbol(sym string) (Token, bool) {
	for _, t := range c.Tokens {
		if strings.EqualFold(t.Symbol, sym) {
			return t, true
		}
	}
	return Token{}, false
}

// TokenByAddress finds a tradeable token by contract address.
func (c *Config) TokenByAddress(addr string) (Token, bool) {
	for _, t := range c.Tokens {
		if strings.EqualFold(t.Address, addr) {
			return t, true
		}
	}
	return Token{}, false
}

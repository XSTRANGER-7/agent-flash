package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nARENA_T_A=from_file\r\nARENA_T_B=\"quoted value\"\nexport ARENA_T_C=x # trailing\nARENA_T_D='single'\n\nnot a line\n"), 0o600)
	t.Setenv("ARENA_T_A", "from_env")
	for _, k := range []string{"ARENA_T_B", "ARENA_T_C", "ARENA_T_D"} {
		os.Unsetenv(k)
		defer os.Unsetenv(k)
	}
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ARENA_T_A": "from_env", "ARENA_T_B": "quoted value", "ARENA_T_C": "x", "ARENA_T_D": "single"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal("a missing .env is fine")
	}
}

func TestStreamURL(t *testing.T) {
	if got := streamURL("https://flash.definitive.fi/v1"); got != "wss://flash.definitive.fi/v1/ws" {
		t.Fatal(got)
	}
}

func TestChainsVerifiedAndTradeable(t *testing.T) {
	t.Setenv("TRADE_CHAINS", "arbitrum,bsc")
	t.Setenv("CHAINS", "")
	t.Setenv("RPC_URL_ARBITRUM", "https://example.invalid/arb")
	c := &Config{Chain: "base", ChainID: 8453, RPCURL: "https://base.example", USDC: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
		WETH: "0x4200000000000000000000000000000000000006", TradeAsset: "WETH"}
	c.buildChains()
	home := c.Home()
	if home.Key != "base" || home.RPC != "https://base.example" || !home.Tradeable() {
		t.Fatalf("home chain comes from the main settings: %+v", home)
	}
	arb, ok := c.ChainByKey("arbitrum")
	if !ok || !arb.Tradeable() || arb.RPC != "https://example.invalid/arb" {
		t.Fatalf("arbitrum: %+v", arb)
	}
	bsc, _ := c.ChainByKey("bsc")
	if bsc.USDC.Decimals != 18 {
		t.Fatal("BNB Chain USDC has 18 decimals (checked onchain)")
	}
	if eth, _ := c.ChainByKey("ethereum"); eth.Tradeable() {
		t.Fatal("TRADE_CHAINS leaves Ethereum out")
	}
	if h, ok := c.ChainByKey("hyperevm"); !ok || h.Tradeable() {
		t.Fatal("HyperEVM is shown (prices) but has no ETH market here")
	}
	seen := map[int64]bool{}
	for _, ch := range c.Chains {
		if seen[ch.ID] || ch.USDC.Address == "" || ch.Explorer == "" {
			t.Fatalf("bad chain entry %+v", ch)
		}
		seen[ch.ID] = true
	}
}

func TestEmptyTradeChainsMeansDefault(t *testing.T) {
	t.Setenv("TRADE_CHAINS", "")
	t.Setenv("CHAINS", "")
	c := &Config{Chain: "base", ChainID: 8453, RPCURL: "x", USDC: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", WETH: "0x4200000000000000000000000000000000000006"}
	c.buildChains()
	n := 0
	for _, ch := range c.Chains {
		if ch.Tradeable() {
			n++
		}
	}
	if n != 8 {
		t.Fatalf("an empty TRADE_CHAINS (copied .env.example) keeps all 8, got %d", n)
	}
	t.Setenv("TRADE_CHAINS", "base")
	c.buildChains()
	if a, _ := c.ChainByKey("arbitrum"); a.Tradeable() {
		t.Fatal("TRADE_CHAINS=base keeps own wallets on Base")
	}
}

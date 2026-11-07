package config

import (
	"os"
	"strings"
)

// Chain is an EVM chain Flash executes on. Every value here was checked on
// Nov 7, 2026: chain names and ids from Flash's supported-chains page, USDC
// from Circle's official address list (BNB Chain: Binance-Peg USDC, 18
// decimals), symbol() and decimals() read onchain through each public RPC,
// and a real Flash quote per chain that returned a FlashOrder with the same
// settlement contract and integrator vault we pin on Base.
type Chain struct {
	Key    string `json:"key"` // Flash chain name ("arbitrum")
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Native string `json:"native"` // gas coin symbol
	RPC    string `json:"-"`      // what the server reads (RPC_URL_<KEY> may hold a provider key)
	// PublicRPC is the chain's public endpoint from Flash's docs: given to
	// the browser for wallet_addEthereumChain, never an operator override.
	PublicRPC string `json:"rpc"`
	Explorer  string `json:"explorer"`
	USDC      Token  `json:"usdc"`
	// ETH is ether as an ERC-20 on this chain. Empty = no ETH market wired
	// here, so the chain is shown (prices, gas) but not traded.
	ETH Token `json:"eth"`
}

// Tradeable: an own-wallet order (USDC -> ETH with a bracket) can be placed.
func (c Chain) Tradeable() bool { return c.ETH.Address != "" }

// NativeIsETH: the gas coin is ether, so its price is the ETH price.
func (c Chain) NativeIsETH() bool { return c.Native == "ETH" }

func usdc(addr string, dec int) Token { return Token{Symbol: "USDC", Address: addr, Decimals: dec} }
func eth(sym, addr string) Token      { return Token{Symbol: sym, Address: addr, Decimals: 18} }

// KnownChains lists the chains in the order the app shows them.
var KnownChains = []Chain{
	{Key: "base", ID: 8453, Name: "Base", Native: "ETH", RPC: "https://mainnet.base.org", Explorer: "https://basescan.org",
		USDC: usdc("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", 6), ETH: eth("WETH", "0x4200000000000000000000000000000000000006")},
	{Key: "ethereum", ID: 1, Name: "Ethereum", Native: "ETH", RPC: "https://ethereum-rpc.publicnode.com", Explorer: "https://etherscan.io",
		USDC: usdc("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", 6), ETH: eth("WETH", "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")},
	{Key: "arbitrum", ID: 42161, Name: "Arbitrum", Native: "ETH", RPC: "https://arb1.arbitrum.io/rpc", Explorer: "https://arbiscan.io",
		USDC: usdc("0xaf88d065e77c8cC2239327C5EDb3A432268e5831", 6), ETH: eth("WETH", "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1")},
	{Key: "optimism", ID: 10, Name: "Optimism", Native: "ETH", RPC: "https://mainnet.optimism.io", Explorer: "https://optimistic.etherscan.io",
		USDC: usdc("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", 6), ETH: eth("WETH", "0x4200000000000000000000000000000000000006")},
	{Key: "polygon", ID: 137, Name: "Polygon", Native: "POL", RPC: "https://polygon-bor-rpc.publicnode.com", Explorer: "https://polygonscan.com",
		USDC: usdc("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", 6), ETH: eth("WETH", "0x7ceB23fD6bC0adD59E62ac25578270cFf1b9f619")},
	{Key: "bsc", ID: 56, Name: "BNB Chain", Native: "BNB", RPC: "https://bsc-dataseed.bnbchain.org", Explorer: "https://bscscan.com",
		USDC: usdc("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d", 18), ETH: eth("ETH", "0x2170Ed0880ac9A755fd29B2688956BD959F933F8")},
	{Key: "avalanche", ID: 43114, Name: "Avalanche", Native: "AVAX", RPC: "https://api.avax.network/ext/bc/C/rpc", Explorer: "https://snowtrace.io",
		USDC: usdc("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", 6), ETH: eth("WETH.e", "0x49D5c2BdFfac6CE2BFdB6640F4F80f226bc10bAB")},
	{Key: "ink", ID: 57073, Name: "Ink", Native: "ETH", RPC: "https://rpc-gel.inkonchain.com", Explorer: "https://explorer.inkonchain.com",
		USDC: usdc("0x2D270e6886d130D724215A266106e6832161EAEd", 6), ETH: eth("WETH", "0x4200000000000000000000000000000000000006")},
	{Key: "hyperevm", ID: 999, Name: "HyperEVM", Native: "HYPE", RPC: "https://rpc.hyperliquid.xyz/evm", Explorer: "https://hyperscan.com",
		USDC: usdc("0xb88339CB7199b77E23DB6E890353E22632Ba630f", 6)},
	{Key: "monad", ID: 143, Name: "Monad", Native: "MON", RPC: "https://rpc.monad.xyz", Explorer: "https://monadvision.com",
		USDC: usdc("0x754704Bc059F8C67012fEd69BC8A327a5aafb603", 6)},
	{Key: "plasma", ID: 9745, Name: "Plasma", Native: "XPL", RPC: "https://rpc.plasma.to", Explorer: "https://plasmascan.to",
		USDC: usdc("0x2d661C89D812261039AF9764eceaAee884f5F67F", 6)},
}

// buildChains applies CHAINS (which to show; default all), TRADE_CHAINS
// (which own wallets may trade on; default every chain with an ETH market)
// and RPC_URL_<KEY> overrides. The home chain always comes from the main
// settings (CHAIN, CHAIN_ID, RPC_URL, USDC, WETH).
func (c *Config) buildChains() {
	show := setOf(list("CHAINS"))
	trade := setOf(list("TRADE_CHAINS")) // empty = every chain with an ETH market; "base" = Base only
	tradeSet := len(trade) > 0
	c.Chains = nil
	for _, k := range KnownChains {
		k.PublicRPC = k.RPC
		if k.Key == c.Chain {
			k.ID, k.RPC = c.ChainID, c.RPCURL
			k.USDC.Address, k.ETH.Address = c.USDC, c.WETH
		} else if len(show) > 0 && !show[k.Key] {
			continue
		}
		if v := strings.TrimSpace(os.Getenv("RPC_URL_" + strings.ToUpper(k.Key))); v != "" {
			k.RPC = v
		}
		if tradeSet && k.Key != c.Chain && !trade[k.Key] {
			k.ETH = Token{} // shown, not traded
		}
		c.Chains = append(c.Chains, k)
	}
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			m[x] = true
		}
	}
	return m
}

// ChainByKey finds a shown chain by its Flash name ("" = the home chain).
func (c *Config) ChainByKey(key string) (Chain, bool) {
	if key == "" {
		key = c.Chain
	}
	for _, ch := range c.Chains {
		if strings.EqualFold(ch.Key, key) {
			return ch, true
		}
	}
	return Chain{}, false
}

// Home is the chain the arena's own wallets and agents trade on (Base).
func (c *Config) Home() Chain {
	ch, ok := c.ChainByKey(c.Chain)
	if !ok {
		return Chain{Key: c.Chain, ID: c.ChainID, Name: c.Chain, Native: "ETH", RPC: c.RPCURL,
			USDC: usdc(c.USDC, 6), ETH: eth(c.TradeAsset, c.WETH)}
	}
	return ch
}

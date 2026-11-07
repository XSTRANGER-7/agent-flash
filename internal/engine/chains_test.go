package engine

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/referee"
)

var arbitrum = config.Chain{Key: "arbitrum", ID: 42161, Name: "Arbitrum", Native: "ETH", RPC: "http://127.0.0.1:1",
	USDC: config.Token{Symbol: "USDC", Address: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", Decimals: 6},
	ETH:  config.Token{Symbol: "WETH", Address: "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", Decimals: 18}}

// The chain id in the payload must be the chain the person picked: an
// Arbitrum order is not signable as a Base one and the other way round.
func TestGuardChecksTheChosenChain(t *testing.T) {
	e, _ := setup(t)
	w, _ := e.Wallets.Get("trendhunter")
	me := w.Address()
	dl := time.Now().Add(time.Hour).Unix()
	arbTD := strings.Replace(flashOrderTD(me, arbitrum.USDC.Address, arbitrum.ETH.Address, "4000000", vault, settlement, dl), `"chainId":"8453"`, `"chainId":"42161"`, 1)
	onArb := expect{from: arbitrum.USDC.Address, to: arbitrum.ETH.Address, exact: big.NewInt(4_000_000), chainID: arbitrum.ID}
	if err := e.Exec.guardTypedData(arbTD, me, onArb); err != nil {
		t.Fatalf("Arbitrum payload refused on Arbitrum: %v", err)
	}
	asBase := onArb
	asBase.chainID = 0
	if err := e.Exec.guardTypedData(arbTD, me, asBase); err == nil {
		t.Fatal("an Arbitrum payload must not pass as a Base order")
	}
	baseTD := flashOrderTD(me, arbitrum.USDC.Address, arbitrum.ETH.Address, "4000000", vault, settlement, dl)
	if err := e.Exec.guardTypedData(baseTD, me, onArb); err == nil {
		t.Fatal("a Base payload must not pass as an Arbitrum order")
	}
	// Same chain, wrong token (Base USDC address on Arbitrum): refused.
	wrongTok := strings.Replace(arbTD, arbitrum.USDC.Address, usdcAddr, 1)
	if err := e.Exec.guardTypedData(wrongTok, me, onArb); err == nil {
		t.Fatal("the chain's own USDC must be the spent token")
	}
}

// Arena wallets (agents, copies, plans) never leave the home chain; another
// chain is only for a person's own wallet, and only for ETH.
func TestOtherChainsOnlyFromOwnWallet(t *testing.T) {
	e, _ := setup(t)
	w, _ := e.Wallets.Get("trendhunter")
	ch := arbitrum
	_, err := e.Exec.Buy(context.Background(), w, 3, 2100, 1950, 0, 2000, BuyOpts{Chain: &ch})
	if err == nil || !strings.Contains(err.Error(), "own wallet") {
		t.Fatalf("an arena wallet order on Arbitrum must be refused, got %v", err)
	}
	_, err = e.Exec.Buy(context.Background(), w, 3, 2100, 1950, 0, 2000, BuyOpts{Chain: &ch, External: true})
	if err == nil || !strings.Contains(err.Error(), "ETH") {
		t.Fatalf("an order on Arbitrum without its ETH token must be refused, got %v", err)
	}
}

func TestSelfPrepareOtherChainRules(t *testing.T) {
	e, _ := setup(t)
	home := e.Cfg.Home()
	hyper := config.Chain{Key: "hyperevm", ID: 999, Name: "HyperEVM", Native: "HYPE", USDC: arbitrum.USDC} // no ETH market
	e.Cfg.Chains = []config.Chain{home, arbitrum, hyper}
	addr := "0x63c3B917fE772FfAb10FA0dfeF90b88B70E5E98d"
	in := referee.Intent{Token: "WETH", SizeUSD: 3, Price: 2000, TakeProfit: 2040, StopLoss: 1980, Style: "market"}

	in.Chain = "hyperevm"
	if _, d, _ := e.SelfPrepare(context.Background(), addr, in); d.OK || d.Code != "CHAIN" {
		t.Fatalf("a chain without an ETH market must be refused: %+v", d)
	}
	in.Chain = "arbitrum"
	in.Token = "cbBTC"
	if _, d, _ := e.SelfPrepare(context.Background(), addr, in); d.OK || d.Code != "TOKEN" {
		t.Fatalf("only ETH trades on another chain: %+v", d)
	}
	in.Token = "WETH"
	e.Cfg.Paper = true
	if _, d, _ := e.SelfPrepare(context.Background(), addr, in); d.OK || d.Code != "CHAIN" || !strings.Contains(d.Reason, "real money") {
		t.Fatalf("paper money stays on the home chain: %+v", d)
	}
}

func TestChange24hNeedsASampleFromADayAgo(t *testing.T) {
	now := time.Now()
	if changeVs(nil, now, 10) != nil {
		t.Fatal("no history must give no change")
	}
	recent := []arena.TapePoint{{T: now.Add(-2 * time.Hour).UnixMilli(), P: 8}}
	if changeVs(recent, now, 10) != nil {
		t.Fatal("a 2 h old sample is not a 24 h change")
	}
	day := []arena.TapePoint{{T: now.Add(-24*time.Hour - 10*time.Minute).UnixMilli(), P: 8}}
	if c := changeVs(day, now, 10); c == nil || *c != 25 {
		t.Fatalf("8 -> 10 is +25%%, got %v", c)
	}
	b := &chainBook{hist: map[string][]arena.TapePoint{}}
	b.sample("BNB", now, 600)
	b.sample("BNB", now.Add(10*time.Minute), 610) // within the hour: not stored again
	if n := len(b.hist["BNB"]); n != 1 {
		t.Fatalf("one sample an hour, got %d", n)
	}
}

// Real setup tx captured from a Flash BNB Chain quote (Nov 7, 2026): an
// unsigned legacy approve(settlement, 5 USDC) with no chain id.
const bscSetupTx = "0xf86a822d038402faf76482fe1d948ac76a51cc950d9822d68b83fe1ad97b32cd580d80b844095ea7b30000000000000000000000005d00000873b6bf41539e6f5365b0ff7d3c368f780000000000000000000000000000000000000000000000004563918244f40000808080"

func TestLegacySetupTxOnBNBChain(t *testing.T) {
	e, _ := setup(t)
	bsc := config.Chain{Key: "bsc", ID: 56, Name: "BNB Chain", Native: "BNB",
		USDC: config.Token{Symbol: "USDC", Address: "0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d", Decimals: 18},
		ETH:  config.Token{Symbol: "ETH", Address: "0x2170Ed0880ac9A755fd29B2688956BD959F933F8", Decimals: 18}}
	if err := e.Exec.checkSetupTxsOn([]string{bscSetupTx}, "0x0", bsc); err != nil {
		t.Fatalf("real BNB Chain approve refused: %v", err)
	}
	if err := e.Exec.checkSetupTxsOn([]string{bscSetupTx}, "0x0", arbitrum); err == nil {
		t.Fatal("a BNB Chain token approve must not pass for an Arbitrum order")
	}
	transfer := strings.Replace(bscSetupTx, "b844095ea7b3", "b844a9059cbb", 1)
	if err := e.Exec.checkSetupTxsOn([]string{transfer}, "0x0", bsc); err == nil {
		t.Fatal("a transfer() setup tx must be refused")
	}
	other := strings.Replace(bscSetupTx, "5d00000873b6bf41539e6f5365b0ff7d3c368f78", "2222222222222222222222222222222222222222", 1)
	if err := e.Exec.checkSetupTxsOn([]string{other}, "0x0", bsc); err == nil {
		t.Fatal("an approve to another spender must be refused")
	}
}

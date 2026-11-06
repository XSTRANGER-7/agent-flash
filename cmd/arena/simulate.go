package main

// `arena simulate`: the onchain half of a trade, run against REAL Base
// mainnet state with TEST money and without changing anything onchain.
//
// Flash has no testnet, and a Flash fill needs real funds. Everything else
// can be proven for free:
//   - our EIP-712 domain matches the live Flash settlement contract
//     (EIP-5267 eip712Domain()) and a real Flash quote's payload;
//   - a fresh wallet gets test USDC through an eth_call state override, and
//     the real USDC contract then runs our capped approve, the settlement
//     pulling up to the cap (and failing one unit above it), a withdrawal to
//     the payout address and a revoke;
//   - a transaction signed by our hand-written EIP-1559 signer is decoded and
//     its sender recovered by a real Base node (it is refused for "insufficient
//     funds", because the fresh wallet has no ETH, so nothing is broadcast).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/engine"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
)

func simulate() error {
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rpc := evm.NewRPC(cfg.RPCURL)
	fc := flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, slog.New(slog.NewTextHandler(io.Discard, nil)))
	settlement := "0x5d00000873b6BF41539e6f5365B0Ff7d3c368f78"
	if len(cfg.SettlementAllowlist) > 0 {
		settlement = cfg.SettlementAllowlist[0]
	}
	fails := 0
	check := func(name string, ok bool, detail string) {
		mark := "pass"
		if !ok {
			mark, fails = "FAIL", fails+1
		}
		fmt.Printf("  %-4s  %-34s %s\n", mark, name, detail)
	}
	fmt.Println("Agent Arena simulation: real Base mainnet state + real Flash quotes, test money, nothing broadcast")

	// A brand-new key: it has never held anything, so nothing can move.
	key, _, err := evm.GenerateKey()
	if err != nil {
		return err
	}
	wallet := key.Address()
	payout := "0x000000000000000000000000000000000000dEaD"
	fmt.Println("  test wallet (fresh, empty):", wallet)

	// 1. The settlement contract's own EIP-712 domain.
	raw, err := rpc.Call(ctx, settlement, []byte{0x84, 0xb0, 0x19, 0x6e})
	if err != nil || len(raw) < 7*32 {
		check("settlement eip712Domain()", false, fmt.Sprint(err))
		return errors.New("cannot read the settlement contract")
	}
	word := func(i int) []byte { return raw[i*32 : i*32+32] }
	str := func(off int) string {
		o := int(new(big.Int).SetBytes(raw[off : off+32]).Int64())
		n := int(new(big.Int).SetBytes(raw[o : o+32]).Int64())
		return string(raw[o+32 : o+32+n])
	}
	name, version := str(32), str(64)
	chainID := new(big.Int).SetBytes(word(3))
	vc := evm.ChecksumAddress(word(4)[12:])
	check("settlement eip712Domain()", chainID.Int64() == cfg.ChainID && strings.EqualFold(vc, settlement),
		fmt.Sprintf("%q v%s chain %s at %s", name, version, chainID, vc))

	// 2. A real Flash quote for the fresh wallet ($5 WETH + bracket).
	no := false
	px := 0.0
	if p, err := arenaPrice(ctx, cfg, rpc); err == nil {
		px = p
	}
	req := &flash.QuoteRequest{TargetChain: cfg.Chain, ContraChain: cfg.Chain, TargetAsset: cfg.WETH, ContraAsset: cfg.USDC,
		Side: "buy", Qty: "5", OrderType: "market", FunderAddress: wallet, MaxSlippage: cfg.MaxSlippage,
		FlashIntegratorFeeBps: cfg.FeeBps, ForceMinimalAllowance: true, EVMUsePermit2: &no}
	if px > 0 {
		req.AttachedBracket = &flash.QuoteBracket{TakeProfit: &flash.BracketLeg{NotionalPrice: fmt.Sprintf("%.2f", px*1.03)},
			StopLoss: &flash.BracketLeg{NotionalPrice: fmt.Sprintf("%.2f", px*0.98)}, ExpireTime: time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
	}
	q, err := fc.Quote(ctx, req)
	if err != nil || q.EVM == nil || q.EVM.OrderTypedData == "" {
		check("real Flash quote", false, fmt.Sprint(err))
		return errors.New("no quote")
	}
	check("real Flash quote", true, fmt.Sprintf("quote %s Â· $5 buys %s WETH", q.QuoteID, q.To.Amount.String()))

	td, err := evm.ParseTypedData(q.EVM.OrderTypedData)
	if err != nil {
		return err
	}
	dn, _ := td.Domain["name"].(string)
	dv, _ := td.Domain["version"].(string)
	dc, _ := evm.ToBigInt(td.Domain["chainId"])
	dvc, _ := td.Domain["verifyingContract"].(string)
	check("quote domain = contract domain", dn == name && dv == version && dc != nil && dc.Cmp(chainID) == 0 && strings.EqualFold(dvc, vc),
		fmt.Sprintf("%q v%s chain %v %s", dn, dv, td.Domain["chainId"], dvc))
	ours, err := td.DomainSeparator()
	typeHash := evm.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	vcb, _ := evm.HexToAddressBytes(vc)
	want := evm.Keccak256(typeHash, evm.Keccak256([]byte(name)), evm.Keccak256([]byte(version)), pad(chainID.Bytes()), pad(vcb))
	check("our domain separator", err == nil && fmt.Sprintf("%x", ours) == fmt.Sprintf("%x", want), fmt.Sprintf("0x%x", ours))
	digest, _ := td.Digest()
	sig, _ := key.Sign(digest)
	who, err := evm.RecoverAddress(digest, sig)
	check("order signature recovers wallet", err == nil && who == wallet, who)

	// 3. Test USDC through a state override (find the balance slot).
	const testUSDC = 25_000_000 // 25 USDC
	usdc := cfg.USDC
	balSlot := -1
	for s := 0; s <= 20 && balSlot < 0; s++ {
		ov := override(usdc, mapSlot(wallet, s), testUSDC)
		out, err := rpc.CallAs(ctx, "", usdc, append(sel("70a08231"), pad(addr(wallet))...), ov)
		if err == nil && new(big.Int).SetBytes(out).Int64() == testUSDC {
			balSlot = s
		}
	}
	check("give wallet 25 test USDC", balSlot >= 0, fmt.Sprintf("USDC balance slot %d", balSlot))
	if balSlot < 0 {
		return fmt.Errorf("%d check(s) failed", fails+1)
	}

	// 4. Our capped approve, as the real USDC contract runs it.
	capUnits := big.NewInt(int64(cfg.MaxApprovalUSD * 1e6))
	if q.EVM.ApproveTx == nil {
		check("Flash asks for an approve", false, "quote had no approveTx")
	} else {
		data, _ := evm.DecodeHex(q.EVM.ApproveTx.Data)
		spender, amount, ok := evm.ParseApprove(data)
		check("Flash approve targets settlement", ok && strings.EqualFold(spender, settlement) && strings.EqualFold(q.EVM.ApproveTx.To, usdc),
			fmt.Sprintf("approve(%s, %s)", spender, amount))
		if amount != nil && amount.Cmp(capUnits) < 0 {
			capUnits = amount
		}
		capped, _ := evm.EncodeApprove(settlement, capUnits)
		out, err := rpc.CallAs(ctx, wallet, usdc, capped, override(usdc, mapSlot(wallet, balSlot), testUSDC))
		check("capped approve runs on USDC", err == nil && isTrue(out), fmt.Sprintf("allowance %s units", capUnits))
	}

	// 5. The cap is enforced by USDC itself: the settlement can pull exactly
	// the allowance and not one unit more.
	allowSlot := -1
	for s := 0; s <= 20 && allowSlot < 0; s++ {
		ov := override(usdc, nestedSlot(wallet, settlement, s), capUnits.Int64())
		out, err := rpc.CallAs(ctx, "", usdc, append(append(sel("dd62ed3e"), pad(addr(wallet))...), pad(addr(settlement))...), ov)
		if err == nil && new(big.Int).SetBytes(out).Cmp(capUnits) == 0 {
			allowSlot = s
		}
	}
	if allowSlot >= 0 {
		both := map[string]any{usdc: map[string]any{"stateDiff": map[string]string{
			mapSlot(wallet, balSlot): word32(testUSDC), nestedSlot(wallet, settlement, allowSlot): word32(capUnits.Int64())}}}
		pull := func(n *big.Int) error {
			d := append(append(append(sel("23b872dd"), pad(addr(wallet))...), pad(addr(settlement))...), pad(n.Bytes())...)
			_, err := rpc.CallAs(ctx, settlement, usdc, d, both)
			return err
		}
		errAt := pull(capUnits)
		errOver := pull(new(big.Int).Add(capUnits, big.NewInt(1)))
		check("settlement pulls the cap", errAt == nil, fmt.Sprintf("%s units ok", capUnits))
		check("one unit over the cap reverts", errOver != nil, shortRPC(errOver))
	} else {
		check("find USDC allowance slot", false, "not found in slots 0-20")
	}

	// 6. Withdraw to the payout address, and revoke.
	tr, _ := evm.EncodeTransfer(payout, big.NewInt(10_000_000))
	out, err := rpc.CallAs(ctx, wallet, usdc, tr, override(usdc, mapSlot(wallet, balSlot), testUSDC))
	check("withdraw 10 USDC to payout", err == nil && isTrue(out), payout)
	tooMuch, _ := evm.EncodeTransfer(payout, big.NewInt(testUSDC+1))
	_, err = rpc.CallAs(ctx, wallet, usdc, tooMuch, override(usdc, mapSlot(wallet, balSlot), testUSDC))
	check("withdraw more than held reverts", err != nil, shortRPC(err))
	rv, _ := evm.EncodeApprove(settlement, big.NewInt(0))
	out, err = rpc.CallAs(ctx, wallet, usdc, rv, nil)
	check("revoke (approve 0) runs on USDC", err == nil && isTrue(out), "")

	// 7. A real Base node decodes our signed EIP-1559 tx and recovers the
	// sender, then refuses it for lack of gas money. Nothing is broadcast.
	if eth, err := rpc.ETHBalance(ctx, wallet); err == nil && eth.Sign() == 0 {
		tip, fee, ferr := rpc.Fees(ctx)
		if ferr == nil {
			capped, _ := evm.EncodeApprove(settlement, capUnits)
			rawTx, hash, err := evm.SignTx(&evm.Tx{ChainID: big.NewInt(cfg.ChainID), Nonce: 0, TipCap: tip, FeeCap: fee, Gas: 70000, To: usdc, Value: big.NewInt(0), Data: capped}, key)
			if err == nil {
				_, err = rpc.SendRaw(ctx, rawTx)
				msg := shortRPC(err)
				ok := err != nil && strings.Contains(strings.ToLower(msg), "insufficient funds")
				check("Base node accepts our tx encoding", ok, fmt.Sprintf("tx %sâ€¦ â†’ %s", hash[:12], msg))
			}
		}
	}

	// 8. Gasless deposit (USDC EIP-3009): the person signs, the gas tank (an
	// address holding no USDC) sends the transfer. Real USDC code via eth_call.
	fmt.Println("  gasless deposit (USDC transferWithAuthorization, real contract):")
	ex := engine.NewExecutor(cfg, fc, rpc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tank, _, _ := evm.GenerateKey()
	sub, _, _ := evm.GenerateKey()
	nb := make([]byte, 32)
	_, _ = rand.Read(nb)
	auth := engine.DepositAuth{From: wallet, To: sub.Address(), Value: big.NewInt(7_000_000),
		ValidBefore: time.Now().Add(time.Hour).Unix(), Nonce: "0x" + hex.EncodeToString(nb)}
	if dtd, err := evm.ParseTypedData(ex.DepositTypedData(auth)); err != nil {
		check("EIP-3009 typed data", false, err.Error())
	} else {
		ds, _ := dtd.DomainSeparator()
		onchainDS, derr := rpc.Call(ctx, usdc, []byte{0x36, 0x44, 0xe5, 0x15})
		check("our deposit domain = USDC's", derr == nil && len(onchainDS) >= 32 && bytes.Equal(ds, onchainDS[:32]), fmt.Sprintf("0x%x", ds))
		dg, _ := dtd.Digest()
		good, _ := key.Sign(dg)
		data, _ := engine.TransferWithAuthCalldata(auth, good)
		_, err := rpc.CallAs(ctx, tank.Address(), usdc, data, override(usdc, mapSlot(wallet, balSlot), testUSDC))
		detail := fmt.Sprintf("7 USDC to %s…, sent by %s… (holds no USDC)", sub.Address()[:10], tank.Address()[:10])
		if err != nil {
			detail = shortRPC(err)
		}
		check("tank sends a signed deposit", err == nil, detail)
		other, _, _ := evm.GenerateKey()
		bad, _ := other.Sign(dg)
		data2, _ := engine.TransferWithAuthCalldata(auth, bad)
		_, err2 := rpc.CallAs(ctx, tank.Address(), usdc, data2, override(usdc, mapSlot(wallet, balSlot), testUSDC))
		check("someone else's signature reverts", err2 != nil, shortRPC(err2))
		big7 := auth
		big7.Value = big.NewInt(70_000_000) // more than the 25 test USDC
		dtd2, _ := evm.ParseTypedData(ex.DepositTypedData(big7))
		dg2, _ := dtd2.Digest()
		sg3, _ := key.Sign(dg2)
		data3, _ := engine.TransferWithAuthCalldata(big7, sg3)
		_, err3 := rpc.CallAs(ctx, tank.Address(), usdc, data3, override(usdc, mapSlot(wallet, balSlot), testUSDC))
		check("deposit above the balance reverts", err3 != nil, shortRPC(err3))
	}

	// 9. Every Flash order type through the same /quote endpoint, each
	// payload checked like the executor checks it before signing.
	fmt.Println("  order types (real Flash quotes, same endpoint):")
	orderTypeQuotes(ctx, cfg, fc, wallet, settlement, px, check)

	if fails > 0 {
		return fmt.Errorf("%d simulation check(s) failed", fails)
	}
	fmt.Println("All simulation checks passed.")
	return nil
}

func arenaPrice(ctx context.Context, cfg *config.Config, rpc *evm.RPC) (float64, error) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	pf := newPriceFeed(cfg, flash.New(cfg.FlashBaseURL, cfg.FlashAPIKey, quiet), rpc, quiet)
	pf.Refresh(ctx)
	if p := pf.Last(); p > 0 {
		return p, nil
	}
	return 0, errors.New("no price")
}

func sel(h string) []byte  { b, _ := evm.DecodeHex("0x" + h); return b }
func addr(a string) []byte { b, _ := evm.HexToAddressBytes(a); return b }
func pad(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
func word32(v int64) string { return fmt.Sprintf("0x%064x", v) }

// mapSlot is the storage key of mapping[key] at slot s (Solidity layout).
func mapSlot(key string, s int) string {
	return fmt.Sprintf("0x%x", evm.Keccak256(pad(addr(key)), pad(big.NewInt(int64(s)).Bytes())))
}

// nestedSlot is mapping[a][b] at slot s.
func nestedSlot(a, b string, s int) string {
	inner := evm.Keccak256(pad(addr(a)), pad(big.NewInt(int64(s)).Bytes()))
	return fmt.Sprintf("0x%x", evm.Keccak256(pad(addr(b)), inner))
}

func override(contract, slot string, v int64) map[string]any {
	return map[string]any{contract: map[string]any{"stateDiff": map[string]string{slot: word32(v)}}}
}

func isTrue(out []byte) bool { return len(out) == 32 && out[31] == 1 }

func shortRPC(err error) string {
	if err == nil {
		return "no error"
	}
	s := err.Error()
	if i := strings.Index(s, "rpc error"); i >= 0 {
		s = s[i:]
	}
	if len(s) > 110 {
		s = s[:110] + "â€¦"
	}
	return s
}

// orderTypeQuotes asks the real Flash API for every advanced order type
// (same /quote endpoint) and checks each FlashOrder payload the way the
// executor does before it signs: chain, contract, wallet, tokens, amount.
func orderTypeQuotes(ctx context.Context, cfg *config.Config, fc *flash.Client, funder, settlement string, px float64, check func(string, bool, string)) {
	if px <= 0 {
		check("order types (no price)", false, "no reference price")
		return
	}
	no := false
	p2 := func(x float64) string { return fmt.Sprintf("%.2f", x) }
	base := func(side, qty, typ string) *flash.QuoteRequest {
		return &flash.QuoteRequest{TargetChain: cfg.Chain, ContraChain: cfg.Chain, TargetAsset: cfg.WETH, ContraAsset: cfg.USDC, Side: side, Qty: qty,
			OrderType: typ, FunderAddress: funder, FlashIntegratorFeeBps: cfg.FeeBps, ForceMinimalAllowance: true, EVMUsePermit2: &no}
	}
	br := func() *flash.QuoteBracket {
		return &flash.QuoteBracket{TakeProfit: &flash.BracketLeg{NotionalPrice: p2(px * 1.03)}, StopLoss: &flash.BracketLeg{NotionalPrice: p2(px * 0.98)},
			ExpireTime: time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)}
	}
	day := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	type tc struct {
		name    string
		req     *flash.QuoteRequest
		bracket bool
	}
	var cases []tc
	m := base("buy", "20", "market")
	m.MaxSlippage, m.AttachedBracket = cfg.MaxSlippage, br()
	cases = append(cases, tc{"Market + bracket (TP/SL)", m, true})
	l := base("buy", "20", "limit")
	l.LimitCrossPrice, l.ExpireTime, l.AttachedBracket = p2(px*0.98), day, br()
	cases = append(cases, tc{"Limit buy + bracket", l, true})
	tw := base("buy", "20", "twap")
	tw.DurationSeconds, tw.TwapBucketCount, tw.AttachedBracket = 600, 10, br()
	cases = append(cases, tc{"DCA / TWAP (10 slices) + bracket", tw, true})
	sb := base("buy", "20", "stop")
	sb.Triggers, sb.ExpireTime = []flash.Trigger{{NotionalPrice: p2(px * 1.01), TriggerType: "upper"}}, day
	cases = append(cases, tc{"Stop buy", sb, false})
	b := base("sell", "0.005", "bracket")
	b.Triggers = []flash.Trigger{{NotionalPrice: p2(px * 0.98), TriggerType: "lower"}, {NotionalPrice: p2(px * 1.03), TriggerType: "upper"}}
	cases = append(cases, tc{"Bracket (standalone sell)", b, false})
	sl := base("sell", "0.005", "stop-loss")
	sl.Triggers = []flash.Trigger{{NotionalPrice: p2(px * 0.98), TriggerType: "lower"}}
	cases = append(cases, tc{"Stop loss (standalone sell)", sl, false})
	tp := base("sell", "0.005", "take-profit")
	tp.Triggers = []flash.Trigger{{NotionalPrice: p2(px * 1.03), TriggerType: "upper"}}
	cases = append(cases, tc{"Take profit (standalone sell)", tp, false})
	ts := base("sell", "0.005", "trailing-stop")
	ts.TrailingStop = &flash.TrailingStop{TrailPercent: "0.02"}
	cases = append(cases, tc{"Trailing stop (sell)", ts, false})

	for _, c := range cases {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		q, err := fc.Quote(cctx, c.req)
		cancel()
		if err != nil {
			check(c.name, false, shortRPC(err))
			continue
		}
		from, to, dec := cfg.USDC, cfg.WETH, 6
		if c.req.Side == "sell" {
			from, to, dec = cfg.WETH, cfg.USDC, 18
		}
		problem := checkFlashOrder(q.EVM, funder, settlement, from, to, c.req.Qty, dec, cfg.ChainID)
		if problem == "" && c.bracket {
			if q.AttachedBracket == nil || q.AttachedBracket.EVM == nil {
				problem = "no attached bracket payload"
			} else if td, err := evm.ParseTypedData(q.AttachedBracket.EVM.OrderTypedData); err != nil || td.PrimaryType != "FlashOrder" ||
				!strings.EqualFold(fmt.Sprint(td.Message["fromToken"]), cfg.WETH) || !strings.EqualFold(fmt.Sprint(td.Message["swapper"]), funder) {
				problem = "bracket payload is not a WETH sell from this wallet"
			}
		}
		detail := "FlashOrder ok: " + q.OrderType
		if c.bracket {
			detail += " + bracket"
		}
		if problem != "" {
			detail = problem
		}
		check(c.name, problem == "", detail)
	}
}

// checkFlashOrder mirrors the executor's never-blind-sign checks.
func checkFlashOrder(x *flash.EVMBlock, funder, settlement, from, to, qty string, dec int, chain int64) string {
	if x == nil || x.OrderTypedData == "" {
		return "no FlashOrder payload"
	}
	td, err := evm.ParseTypedData(x.OrderTypedData)
	if err != nil {
		return err.Error()
	}
	cid, _ := evm.ToBigInt(td.Domain["chainId"])
	vc, _ := td.Domain["verifyingContract"].(string)
	str := func(k string) string { s, _ := td.Message[k].(string); return s }
	amt, _ := evm.ToBigInt(td.Message["fromAmount"])
	want, _ := new(big.Float).Mul(mustFloat(qty), new(big.Float).SetFloat64(math.Pow10(dec))).Int(nil)
	dl, _ := evm.ToBigInt(td.Message["deadline"])
	switch {
	case td.PrimaryType != "FlashOrder":
		return "primaryType " + td.PrimaryType
	case cid == nil || cid.Int64() != chain:
		return "wrong chain"
	case !strings.EqualFold(vc, settlement):
		return "verifyingContract is not the settlement contract"
	case !strings.EqualFold(str("swapper"), funder) || !strings.EqualFold(str("recipient"), funder):
		return "swapper/recipient is not this wallet"
	case !strings.EqualFold(str("fromToken"), from) || !strings.EqualFold(str("toToken"), to):
		return "wrong tokens"
	case amt == nil || new(big.Int).Sub(amt, want).CmpAbs(big.NewInt(1)) > 0:
		return fmt.Sprintf("amount %v, expected %v", amt, want)
	case dl == nil || dl.Int64() <= time.Now().Unix():
		return "deadline in the past"
	}
	return ""
}

func mustFloat(s string) *big.Float { f, _ := new(big.Float).SetString(s); return f }

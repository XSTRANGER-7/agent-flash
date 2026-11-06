package engine

// Gasless for people: they hold only USDC.
//
//   - Flash pays the gas of every order it executes (it is inside the quote).
//   - The few transactions a sub-wallet must send itself (token approvals,
//     withdrawals, revokes) get their gas from the gas tank: when the wallet
//     cannot pay, the tank first sends it a little ETH (a "drip").
//   - USDC deposits from a person's own wallet are EIP-3009
//     transferWithAuthorization signatures (USDC on Base supports them): the
//     person signs in their wallet, the tank submits the transfer and pays.
//
// The tank only spends what GAS_TANK_DAILY_ETH allows per day and drips each
// wallet at most GAS_DRIPS_PER_WALLET_DAY times a day. Paper and dry-run modes
// send nothing onchain, so the tank is not used there.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/wallet"
)

type gasTank struct {
	w     *wallet.Wallet
	mu    sync.Mutex // one tank transaction at a time (its nonce)
	day   string
	spent *big.Int       // wei committed today
	drips map[string]int // drips per wallet today
}

// SetGasTank turns the tank on (live mode only does anything).
func (e *Executor) SetGasTank(w *wallet.Wallet) {
	if w == nil || w.Signer.Key() == nil {
		e.tank = nil
		return
	}
	e.tank = &gasTank{w: w, spent: new(big.Int), drips: map[string]int{}}
}

// GasStatus is what the app shows about the tank.
type GasStatus struct {
	On         bool    `json:"on"`
	Address    string  `json:"address,omitempty"`
	ETH        float64 `json:"eth"`
	SpentToday float64 `json:"spentTodayEth"`
	DailyCap   float64 `json:"dailyCapEth"`
	Live       bool    `json:"live"` // the tank only sends anything in live mode
}

func (e *Executor) GasTankStatus(ctx context.Context) GasStatus {
	t := e.tank
	if t == nil {
		return GasStatus{Live: e.cfg.Live()}
	}
	st := GasStatus{On: true, Address: t.w.Address(), DailyCap: e.cfg.GasDailyETH, Live: e.cfg.Live()}
	if bal, err := e.rpc.ETHBalance(ctx, t.w.Address()); err == nil {
		st.ETH = weiToETH(bal)
	}
	t.mu.Lock()
	if t.day == today() {
		st.SpentToday = weiToETH(t.spent)
	}
	t.mu.Unlock()
	return st
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

func ethToWei(x float64) *big.Int {
	f := new(big.Float).Mul(big.NewFloat(x), big.NewFloat(1e18))
	i, _ := f.Int(nil)
	return i
}

func weiToETH(w *big.Int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(w), big.NewFloat(1e18)).Float64()
	return f
}

// reserve checks the daily limits for `wei` more (t.mu held).
func (t *gasTank) reserve(dailyETH float64, perWallet int, walletID string, wei *big.Int, drip bool) error {
	if d := today(); t.day != d {
		t.day, t.spent, t.drips = d, new(big.Int), map[string]int{}
	}
	if new(big.Int).Add(t.spent, wei).Cmp(ethToWei(dailyETH)) > 0 {
		return fmt.Errorf("gas tank daily limit reached (%.6f ETH); try again tomorrow or raise GAS_TANK_DAILY_ETH", dailyETH)
	}
	if drip && t.drips[walletID] >= perWallet {
		return fmt.Errorf("gas tank already topped up %s %d times today (GAS_DRIPS_PER_WALLET_DAY)", walletID, perWallet)
	}
	return nil
}

func (t *gasTank) book(walletID string, wei *big.Int, drip bool) {
	t.spent.Add(t.spent, wei)
	if drip {
		t.drips[walletID]++
	}
}

// tankErr makes the common failure readable.
func (e *Executor) tankErr(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "insufficient funds") {
		return fmt.Errorf("the gas tank is out of ETH: send a little ETH on Base to %s", e.tank.w.Address())
	}
	return err
}

// ensureGas makes sure w can pay for the transaction (to, data) it is about
// to send, topping it up from the tank when it cannot. No-op without a tank
// or outside live mode.
func (e *Executor) ensureGas(ctx context.Context, w *wallet.Wallet, to string, data []byte) error {
	t := e.tank
	if t == nil || !e.cfg.Live() || strings.EqualFold(w.Address(), t.w.Address()) {
		return nil
	}
	gas, err := e.rpc.EstimateGas(ctx, w.Address(), to, data, nil)
	if err != nil || gas == 0 {
		gas = 120_000 // the transaction's own error, if any, surfaces when it is sent
	}
	_, feeCap, err := e.rpc.Fees(ctx)
	if err != nil {
		return fmt.Errorf("gas price: %w", err)
	}
	// SendTx signs gas*1.2 at feeCap; the node wants that much ETH up front.
	need := new(big.Int).Mul(feeCap, new(big.Int).SetUint64(gas*12/10))
	bal, err := e.rpc.ETHBalance(ctx, w.Address())
	if err != nil {
		return fmt.Errorf("read %s's ETH: %w", w.ID, err)
	}
	if bal.Cmp(need) >= 0 {
		return nil
	}
	drip := ethToWei(e.cfg.GasDripETH)
	if twice := new(big.Int).Mul(need, big.NewInt(2)); drip.Cmp(twice) < 0 {
		drip = twice
	}
	cost := new(big.Int).Add(drip, new(big.Int).Mul(feeCap, big.NewInt(25_200))) // + the tank's own transfer gas
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.reserve(e.cfg.GasDailyETH, e.cfg.GasDripsPerWalletDay, w.ID, cost, true); err != nil {
		return err
	}
	h, err := e.rpc.SendTx(ctx, big.NewInt(e.cfg.ChainID), t.w.Signer.Key(), w.Address(), nil, drip)
	if err != nil {
		return fmt.Errorf("gas tank could not top up %s: %w", w.ID, e.tankErr(err))
	}
	t.book(w.ID, cost, true)
	e.log.Info("gas tank topped up a wallet", "wallet", w.ID, "eth", weiToETH(drip), "tx", h)
	return nil
}

// ---- gasless USDC deposits (EIP-3009) ------------------------------------

// DepositAuth is one USDC transferWithAuthorization: From signs, the tank sends.
type DepositAuth struct {
	From        string
	To          string
	Value       *big.Int // USDC base units (6 decimals)
	ValidAfter  int64
	ValidBefore int64
	Nonce       string // 0x + 64 hex, random
}

var transferWithAuthSel = evm.Keccak256([]byte("transferWithAuthorization(address,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)"))[:4]

// DepositTypedData is the EIP-712 payload the person signs
// (eth_signTypedData_v4). USDC on Base: name "USD Coin", version "2".
func (e *Executor) DepositTypedData(a DepositAuth) string {
	f := func(n, t string) map[string]string { return map[string]string{"name": n, "type": t} }
	td := map[string]any{
		"types": map[string]any{
			"EIP712Domain": []any{f("name", "string"), f("version", "string"), f("chainId", "uint256"), f("verifyingContract", "address")},
			"TransferWithAuthorization": []any{f("from", "address"), f("to", "address"), f("value", "uint256"),
				f("validAfter", "uint256"), f("validBefore", "uint256"), f("nonce", "bytes32")},
		},
		"primaryType": "TransferWithAuthorization",
		"domain":      map[string]any{"name": "USD Coin", "version": "2", "chainId": e.cfg.ChainID, "verifyingContract": e.cfg.USDC},
		"message": map[string]any{"from": a.From, "to": a.To, "value": a.Value.String(),
			"validAfter": strconv.FormatInt(a.ValidAfter, 10), "validBefore": strconv.FormatInt(a.ValidBefore, 10), "nonce": a.Nonce},
	}
	b, _ := json.Marshal(td)
	return string(b)
}

// VerifyDeposit checks the signature and the authorization's shape.
func (e *Executor) VerifyDeposit(a DepositAuth, sig string) error {
	switch {
	case a.Value == nil || a.Value.Sign() <= 0:
		return errors.New("amount must be positive")
	case len(a.Nonce) != 66 || !strings.HasPrefix(a.Nonce, "0x"):
		return errors.New("bad authorization nonce")
	case time.Now().Unix() >= a.ValidBefore:
		return errors.New("this deposit request expired; start again")
	}
	if err := recoversTo(e.DepositTypedData(a), sig, a.From); err != nil {
		return fmt.Errorf("signature check failed: %w", err)
	}
	return nil
}

// checkUSDCDomain compares our EIP-712 domain with USDC's own
// DOMAIN_SEPARATOR() once, so a signature can never be for the wrong domain.
func (e *Executor) checkUSDCDomain(ctx context.Context) error {
	if e.usdcDomainOK.Load() {
		return nil
	}
	out, err := e.rpc.Call(ctx, e.cfg.USDC, []byte{0x36, 0x44, 0xe5, 0x15}) // DOMAIN_SEPARATOR()
	if err != nil || len(out) < 32 {
		return fmt.Errorf("read USDC domain: %v", err)
	}
	td, err := evm.ParseTypedData(e.DepositTypedData(DepositAuth{From: e.cfg.USDC, To: e.cfg.USDC, Value: big.NewInt(1), Nonce: "0x" + strings.Repeat("00", 32)}))
	if err != nil {
		return err
	}
	ours, err := td.DomainSeparator()
	if err != nil {
		return err
	}
	if !bytes.Equal(out[:32], ours) {
		return fmt.Errorf("USDC's EIP-712 domain is 0x%x, ours is 0x%x; refusing gasless deposits", out[:32], ours)
	}
	e.usdcDomainOK.Store(true)
	return nil
}

// TransferWithAuthCalldata encodes USDC transferWithAuthorization(from, to, value,
// validAfter, validBefore, nonce, v, r, s) for a 65-byte r||s||v signature.
func TransferWithAuthCalldata(a DepositAuth, sig []byte) ([]byte, error) {
	from, err := evm.HexToAddressBytes(a.From)
	if err != nil {
		return nil, err
	}
	to, err := evm.HexToAddressBytes(a.To)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(strings.TrimPrefix(a.Nonce, "0x"))
	if err != nil || len(nonce) != 32 || len(sig) != 65 {
		return nil, errors.New("bad nonce or signature length")
	}
	v := sig[64]
	if v < 27 {
		v += 27
	}
	w := func(b []byte) []byte { out := make([]byte, 32); copy(out[32-len(b):], b); return out }
	data := append([]byte{}, transferWithAuthSel...)
	for _, part := range [][]byte{w(from), w(to), w(a.Value.Bytes()), w(big.NewInt(a.ValidAfter).Bytes()), w(big.NewInt(a.ValidBefore).Bytes()),
		nonce, w([]byte{v}), sig[0:32], sig[32:64]} {
		data = append(data, part...)
	}
	return data, nil
}

// Deposit moves USDC from a person's own wallet into one of their arena
// wallets without gas. Live: the tank relays the signed EIP-3009 transfer.
// Paper: the signature is checked exactly the same way, then paper USDC is
// credited (the person's real wallet is not touched). Dry: checked only.
// The deposit is booked as a flow, so it never counts as profit.
func (e *Engine) Deposit(ctx context.Context, walletID string, a DepositAuth, sig string) (string, error) {
	w, ok := e.Wallets.Get(walletID)
	if !ok {
		return "", errors.New("wallet not found")
	}
	if !strings.EqualFold(w.Address(), a.To) {
		return "", errors.New("the deposit is not addressed to this wallet")
	}
	usd, _ := new(big.Float).Quo(new(big.Float).SetInt(a.Value), big.NewFloat(1e6)).Float64()
	var tx string
	switch {
	case e.Cfg.Paper:
		if err := e.Exec.VerifyDeposit(a, sig); err != nil {
			return "", err
		}
		if e.Venue == nil {
			return "", errors.New("paper venue not running")
		}
		e.Venue.Deposit(w.Address(), e.Cfg.USDC, usd)
		tx = "paper"
	case e.Cfg.DryRun:
		if err := e.Exec.VerifyDeposit(a, sig); err != nil {
			return "", err
		}
		return "dryrun", nil
	default:
		h, err := e.Exec.RelayDeposit(ctx, a, sig)
		if err != nil {
			return h, err
		}
		tx = h
	}
	e.InvalidateBalance(w.ID)
	e.Store.AddFlow(w.ID, usd)
	_ = e.Store.Save()
	return tx, nil
}

// RelayDeposit submits a signed USDC transferWithAuthorization from the gas
// tank (live mode). It simulates the call first, so a transfer that would
// fail (not enough USDC, used nonce) costs nothing.
func (e *Executor) RelayDeposit(ctx context.Context, a DepositAuth, sig string) (string, error) {
	if err := e.VerifyDeposit(a, sig); err != nil {
		return "", err
	}
	if !e.cfg.Live() {
		return "", errors.New("gasless deposits are sent in live mode only")
	}
	t := e.tank
	if t == nil {
		return "", errors.New("gasless deposits need the gas tank: run `arena gastank` and fund it with a little ETH")
	}
	if err := e.checkUSDCDomain(ctx); err != nil {
		return "", err
	}
	sb, err := evm.DecodeHex(sig)
	if err != nil {
		return "", err
	}
	data, err := TransferWithAuthCalldata(a, sb)
	if err != nil {
		return "", err
	}
	if _, err := e.rpc.CallAs(ctx, t.w.Address(), e.cfg.USDC, data, nil); err != nil {
		return "", fmt.Errorf("the transfer would fail onchain (%s); check your USDC balance on Base", shortErr(err))
	}
	gas, err := e.rpc.EstimateGas(ctx, t.w.Address(), e.cfg.USDC, data, nil)
	if err != nil || gas == 0 {
		gas = 120_000
	}
	_, feeCap, err := e.rpc.Fees(ctx)
	if err != nil {
		return "", fmt.Errorf("gas price: %w", err)
	}
	cost := new(big.Int).Mul(feeCap, new(big.Int).SetUint64(gas*12/10))
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.reserve(e.cfg.GasDailyETH, e.cfg.GasDripsPerWalletDay, "", cost, false); err != nil {
		return "", err
	}
	h, err := e.rpc.SendTx(ctx, big.NewInt(e.cfg.ChainID), t.w.Signer.Key(), e.cfg.USDC, data, nil)
	if err != nil {
		return h, fmt.Errorf("deposit: %w", e.tankErr(err))
	}
	t.book("", cost, false)
	e.log.Info("gasless deposit relayed", "from", a.From, "to", a.To, "usdc", a.Value.String(), "tx", h)
	return h, nil
}

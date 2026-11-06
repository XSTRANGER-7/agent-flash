package engine

import (
	"context"
	"encoding/hex"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/wallet"
)

func signDeposit(t *testing.T, e *Executor, key *evm.PrivateKey, a DepositAuth) string {
	t.Helper()
	td, err := evm.ParseTypedData(e.DepositTypedData(a))
	if err != nil {
		t.Fatal(err)
	}
	d, err := td.Digest()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := key.Sign(d)
	if err != nil {
		t.Fatal(err)
	}
	return "0x" + hex.EncodeToString(sig)
}

// A USDC EIP-3009 deposit is accepted only with the owner's signature over
// exactly the authorization the server issued, before it expires.
func TestGaslessDepositSignatureChecks(t *testing.T) {
	e := NewExecutor(&config.Config{ChainID: 8453, USDC: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"}, nil, nil, nil)
	user, _, _ := evm.GenerateKey()
	other, _, _ := evm.GenerateKey()
	a := DepositAuth{From: user.Address(), To: "0x1111111111111111111111111111111111111111", Value: big.NewInt(5_000_000),
		ValidBefore: time.Now().Add(time.Hour).Unix(), Nonce: "0x" + strings.Repeat("ab", 32)}
	sig := signDeposit(t, e, user, a)
	if err := e.VerifyDeposit(a, sig); err != nil {
		t.Fatalf("good signature refused: %v", err)
	}
	more := a
	more.Value = big.NewInt(50_000_000)
	if e.VerifyDeposit(more, sig) == nil {
		t.Fatal("a changed amount was accepted")
	}
	elsewhere := a
	elsewhere.To = "0x2222222222222222222222222222222222222222"
	if e.VerifyDeposit(elsewhere, sig) == nil {
		t.Fatal("a changed destination was accepted")
	}
	if e.VerifyDeposit(a, signDeposit(t, e, other, a)) == nil {
		t.Fatal("someone else's signature was accepted")
	}
	old := a
	old.ValidBefore = time.Now().Add(-time.Minute).Unix()
	if e.VerifyDeposit(old, signDeposit(t, e, user, old)) == nil {
		t.Fatal("an expired authorization was accepted")
	}
	// Calldata: selector + 9 words, v as 27/28.
	raw, _ := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
	data, err := TransferWithAuthCalldata(a, raw)
	if err != nil || len(data) != 4+9*32 || hex.EncodeToString(data[:4]) != "e3ee160e" {
		t.Fatalf("calldata %d bytes, selector %x, err %v", len(data), data[:4], err)
	}
	if v := data[4+6*32+31]; v != 27 && v != 28 {
		t.Fatalf("v = %d", v)
	}
}

// Outside live mode the tank never sends anything (a nil RPC would panic if
// it tried), and the tank is kept out of the trading wallets.
func TestGasTankIdleOutsideLiveAndNeverListed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallets.json")
	if _, err := wallet.Generate(path, []string{"trendhunter"}, nil); err != nil {
		t.Fatal(err)
	}
	addr, created, err := wallet.EnsureGasTank(path)
	if err != nil || !created || !strings.HasPrefix(addr, "0x") {
		t.Fatalf("EnsureGasTank: %v %v %s", err, created, addr)
	}
	if again, created2, _ := wallet.EnsureGasTank(path); created2 || again != addr {
		t.Fatal("a second call must keep the same tank")
	}
	ws, err := wallet.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tank, ok := ws.GasTank()
	if !ok || tank.Address() != addr {
		t.Fatal("tank not loaded")
	}
	for _, w := range ws.List("") {
		if strings.EqualFold(w.Address(), addr) {
			t.Fatal("the tank must not be listed as a trading wallet")
		}
	}
	if !ws.IsArenaAddress(addr) {
		t.Fatal("the tank counts as an arena address (never a payout)")
	}
	agent, _ := ws.Get("trendhunter")
	for _, mode := range []string{"paper", "dry"} {
		e := NewExecutor(&config.Config{ChainID: 8453, Mode: mode, Paper: mode == "paper", DryRun: mode == "dry"}, nil, nil, nil)
		e.SetGasTank(tank)
		if err := e.ensureGas(context.Background(), agent, "", nil); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

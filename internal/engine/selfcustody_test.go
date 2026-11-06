package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/wallet"
)

func signTD(t *testing.T, k *evm.PrivateKey, td string) string {
	t.Helper()
	p, err := evm.ParseTypedData(td)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := p.Digest()
	sig, err := k.Sign(d)
	if err != nil {
		t.Fatal(err)
	}
	return evm.SigHex(sig)
}

func TestSelfCustodyOrderSignedInOwnWallet(t *testing.T) {
	e, ff, v, _ := paperSetup(t)
	e.Venue = v
	ctx := context.Background()
	user, _, _ := evm.GenerateKey() // the person's own wallet (we never see this key in production)
	in := intent()
	in.WalletID, in.SizeUSD, in.Reason = "", 5, "from my own wallet"
	p, d, err := e.SelfPrepare(ctx, user.Address(), in)
	if err != nil || !d.OK || p == nil || p.External == nil {
		t.Fatalf("prepare: %v %+v %+v", err, d, p)
	}
	x := p.External
	if x.EntryTypedData == "" || x.BracketTypedData == "" || len(x.Approvals) != 0 { // paper: no onchain approvals
		t.Fatalf("external payload: %+v", x)
	}
	if ff.orders.Load() != 0 {
		t.Fatal("nothing may be submitted before the wallet signs")
	}
	// A signature from someone else is refused.
	stranger, _, _ := evm.GenerateKey()
	res, err := e.SelfSubmit(ctx, x.QuoteID, user.Address(), signTD(t, stranger, x.EntryTypedData), signTD(t, stranger, x.BracketTypedData))
	if err != nil || res.Trade != nil || !strings.Contains(res.Error, "signed by") {
		t.Fatalf("forged signature accepted: %+v %v", res, err)
	}
	// A fresh quote, signed by the owner, goes through.
	p, _, _ = e.SelfPrepare(ctx, user.Address(), in)
	x = p.External
	res, err = e.SelfSubmit(ctx, x.QuoteID, user.Address(), signTD(t, user, x.EntryTypedData), signTD(t, user, x.BracketTypedData))
	if err != nil || res.Trade == nil || res.Trade.Kind != "self" {
		t.Fatalf("submit: %+v %v", res, err)
	}
	e.RefreshTrade(ctx, res.Trade.ID)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "filled" || tr.BracketOrderID == "" || v.Balance(user.Address(), usdcAddr) >= 20 {
		t.Fatalf("after fill: %s bracket %q", tr.Status, tr.BracketOrderID)
	}
	// The arena can never sign for a self-custody wallet (exit needs the owner).
	w, _ := e.Wallets.Get(tr.WalletID)
	if _, err := w.Signer.SignPersonal("x"); !errors.Is(err, wallet.ErrSelfCustody) {
		t.Fatalf("self wallet signed: %v", err)
	}
	if got := e.LiveSpent(); got != 0 {
		t.Fatalf("a person's own money counted in the arena budget: %v", got)
	}
	// Stop buys need a second signature after the fill: refused up front.
	in.Style, in.StopPrice = "stop", 3100
	if _, d, _ := e.SelfPrepare(ctx, user.Address(), in); d.OK {
		t.Fatal("stop buy allowed from a self-custody wallet")
	}
}

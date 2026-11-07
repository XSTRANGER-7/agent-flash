package engine

// Self-custody orders: the person signs in their own wallet (MetaMask,
// Coinbase Wallet, Rabby, an embedded wallet); this service never holds the
// key. It quotes, runs every guard, hands back the exact FlashOrder payloads
// plus capped approvals, then checks the returned signatures recover to the
// person's address before submitting to Flash.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/wallet"
)

// ExternalOrder is what a self-custody wallet signs.
type ExternalOrder struct {
	QuoteID          string       `json:"quoteId"`
	Chain            string       `json:"chain"`   // Flash chain name: the wallet signs and approves there
	ChainID          int64        `json:"chainId"` // EIP-155 id (the typed data says the same)
	EntryTypedData   string       `json:"entryTypedData"`
	BracketTypedData string       `json:"bracketTypedData,omitempty"`
	Approvals        []ExternalTx `json:"approvals"` // send these first (capped ERC-20 approvals)
	ExpiresAt        time.Time    `json:"expiresAt"`
}

// ExternalTx is a transaction for the person's wallet to send.
type ExternalTx struct {
	To     string `json:"to"`
	Data   string `json:"data"`
	Value  string `json:"value"`
	Label  string `json:"label"`
	Token  string `json:"token"`
	Amount string `json:"amount"` // base units
}

type extState struct {
	req     *flash.QuoteRequest
	q       *flash.QuoteResponse
	funder  string
	bracket bool
	p       *Placement
	at      time.Time
}

var (
	extMu sync.Mutex
	ext   = map[string]*extState{}
)

// capApproveData re-encodes Flash's approve with min(requested, cap), after
// the same checks cappedApprove runs (token, no ETH value, pinned spender).
func (e *Executor) capApproveData(tx *flash.EVMTx, token string, decimals int, capUnits float64) ([]byte, string, error) {
	if !strings.EqualFold(tx.To, token) {
		return nil, "", fmt.Errorf("approve tx targets %s, expected token %s", tx.To, token)
	}
	if tx.Value != "" && tx.Value != "0" && tx.Value != "0x0" {
		return nil, "", errors.New("approve tx carries ETH value; refusing")
	}
	data, err := evm.DecodeHex(tx.Data)
	if err != nil {
		return nil, "", err
	}
	spender, amount, ok := evm.ParseApprove(data)
	if !ok || !e.settlement.has(spender) {
		return nil, "", errors.New("approve is not to the pinned Flash settlement contract; refusing")
	}
	if c := toUnits(capUnits, decimals); amount.Cmp(c) > 0 {
		amount = c
	}
	out, err := evm.EncodeApprove(spender, amount)
	return out, amount.String(), err
}

func (e *Executor) prepareExternal(w *wallet.Wallet, req *flash.QuoteRequest, q *flash.QuoteResponse, bracket bool, usd float64, tok config.Token, ch config.Chain, p *Placement) (*Placement, error) {
	x := &ExternalOrder{QuoteID: q.QuoteID, Chain: ch.Key, ChainID: ch.ID, EntryTypedData: q.EVM.OrderTypedData, ExpiresAt: time.Now().Add(4 * time.Minute).UTC(), Approvals: []ExternalTx{}}
	if bracket {
		x.BracketTypedData = q.AttachedBracket.EVM.OrderTypedData
	}
	if !e.cfg.Paper { // paper money needs no onchain allowance
		if q.EVM.ApproveTx != nil {
			data, amt, err := e.capApproveData(q.EVM.ApproveTx, ch.USDC.Address, ch.USDC.Decimals, math.Min(usd, e.cfg.MaxApprovalUSD))
			if err != nil {
				return nil, err
			}
			x.Approvals = append(x.Approvals, ExternalTx{To: ch.USDC.Address, Data: fmt.Sprintf("0x%x", data), Value: "0x0", Token: "USDC", Amount: amt,
				Label: fmt.Sprintf("Allow Flash to spend $%.2f USDC (this order only)", math.Min(usd, e.cfg.MaxApprovalUSD))})
		}
		if bracket && q.AttachedBracket.EVM.ApproveTx != nil {
			capTok := q.AttachedBracket.SignedMaxFromAmount.Float() * 1.01
			data, amt, err := e.capApproveData(q.AttachedBracket.EVM.ApproveTx, tok.Address, tok.Decimals, capTok)
			if err != nil {
				return nil, err
			}
			x.Approvals = append(x.Approvals, ExternalTx{To: tok.Address, Data: fmt.Sprintf("0x%x", data), Value: "0x0", Token: tok.Symbol, Amount: amt,
				Label: fmt.Sprintf("Allow Flash to sell up to %.8f %s for your take profit / stop loss", capTok, tok.Symbol)})
		}
	}
	p.External = x
	p.step("ready_to_sign", "payload checked; waiting for your wallet's signature")
	extMu.Lock()
	for id, s := range ext {
		if time.Since(s.at) > 10*time.Minute {
			delete(ext, id)
		}
	}
	ext[q.QuoteID] = &extState{req: req, q: q, funder: w.Address(), bracket: bracket, p: p, at: time.Now()}
	extMu.Unlock()
	return p, nil
}

func recoversTo(typedData, sig, addr string) error {
	td, err := evm.ParseTypedData(typedData)
	if err != nil {
		return err
	}
	d, err := td.Digest()
	if err != nil {
		return err
	}
	b, err := evm.DecodeHex(sig)
	if err != nil {
		return err
	}
	who, err := evm.RecoverAddress(d, b)
	if err != nil {
		return err
	}
	if !strings.EqualFold(who, addr) {
		return fmt.Errorf("signed by %s, not %s", who, addr)
	}
	return nil
}

// SubmitExternal checks the wallet's signatures and sends the order to Flash.
func (e *Executor) SubmitExternal(ctx context.Context, quoteID, funder, entrySig, bracketSig string) (*Placement, error) {
	extMu.Lock()
	st := ext[quoteID]
	delete(ext, quoteID)
	extMu.Unlock()
	switch {
	case st == nil:
		return nil, errors.New("quote not found; ask for a new one")
	case time.Since(st.at) > 4*time.Minute:
		return nil, errors.New("quote expired; ask for a new one")
	case !strings.EqualFold(st.funder, funder):
		return nil, errors.New("this quote belongs to another wallet")
	}
	if err := recoversTo(st.q.EVM.OrderTypedData, entrySig, funder); err != nil {
		return nil, fmt.Errorf("entry signature: %w", err)
	}
	order := flash.NewOrderRequest(st.req, quoteID, entrySig, st.q.EVM.OrderTypedData)
	order.AttributionCode = e.cfg.AttributionCode
	if st.bracket {
		if err := recoversTo(st.q.AttachedBracket.EVM.OrderTypedData, bracketSig, funder); err != nil {
			return nil, fmt.Errorf("bracket signature: %w", err)
		}
		ab := st.q.AttachedBracket
		order.AttachedBracket = &flash.SubmitBracket{TakeProfit: st.req.AttachedBracket.TakeProfit, StopLoss: st.req.AttachedBracket.StopLoss,
			UserSignature: bracketSig, Deadline: ab.Deadline, SignedMaxFromAmount: ab.SignedMaxFromAmount.String(), Salt: ab.Salt}
	}
	p := st.p
	p.External = nil
	p.step("signed", "signed in your own wallet (EIP-712); signatures verified to "+funder)
	if e.cfg.DryRun {
		p.OrderID = "dryrun-" + quoteID
		p.step("dry_run", "not submitted (dry run)")
		return p, nil
	}
	res, err := e.flash.SubmitOrder(ctx, order)
	metrics.Inc("flash_orders_total")
	if err != nil {
		metrics.Inc("flash_order_errors_total")
		return nil, fmt.Errorf("submit: %w", err)
	}
	p.OrderID = res.OrderID
	p.step("submitted", "Flash order "+res.OrderID+" from your wallet (bracket activates on the first fill)")
	return p, nil
}

package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/wallet"
)

type selfPending struct {
	in   referee.Intent
	w    *wallet.Wallet
	d    referee.Decision
	usdc float64
	ref  float64
	tok  config.Token
	at   time.Time
}

var (
	selfMu  sync.Mutex
	selfQue = map[string]selfPending{}
)

// SelfPrepare quotes and checks an order for a person's own wallet (same
// referee and guards as every arena order) and returns what to sign.
func (e *Engine) SelfPrepare(ctx context.Context, addr string, in referee.Intent) (*Placement, referee.Decision, error) {
	w, err := e.Wallets.Watch(addr)
	if err != nil {
		return nil, referee.Decision{}, err
	}
	e.Store.AddSelfWallet(w.ID, w.Address())
	in.WalletID, in.OwnSize = w.ID, true
	if d := e.walletGate(w.ID); !d.OK {
		return nil, d, nil
	}
	if in.Style == "stop" {
		return nil, deny("ORDER_TYPE", "stop buys need a second signature after the fill; from your own wallet use market, limit or DCA"), nil
	}
	// Another chain: real money only (paper money lives on the home chain),
	// ETH only, so the onchain ETH price still checks Flash's price.
	ch := e.Cfg.Home()
	if in.Chain != "" && !strings.EqualFold(in.Chain, ch.Key) {
		c, ok := e.Cfg.ChainByKey(in.Chain)
		switch {
		case !ok || !c.Tradeable():
			return nil, deny("CHAIN", "the arena does not trade on "+in.Chain), nil
		case e.Cfg.Paper:
			return nil, deny("CHAIN", c.Name+" orders use real money; this server runs paper money, which lives on "+ch.Name), nil
		case !strings.EqualFold(in.Token, e.Cfg.TradeAsset) && !strings.EqualFold(in.Token, "ETH") && !strings.EqualFold(in.Token, c.ETH.Symbol):
			return nil, deny("TOKEN", "on "+c.Name+" the arena trades ETH"), nil
		}
		ch = c
		in.Token, in.Chain, in.ChainToken = e.Cfg.TradeAsset, c.Key, c.ETH.Address
	} else {
		in.Chain, in.ChainToken = "", ""
	}
	foreign := in.Chain != ""
	var usdc float64
	if foreign {
		var cb ChainBalance
		cb, err = e.ChainBalances(ctx, ch, w.Address())
		usdc = cb.USDC
	} else {
		usdc, _, err = e.Balances(ctx, w, 0)
	}
	if err != nil {
		return nil, referee.Decision{}, err
	}
	tok, _ := e.tokenBySymbol(in.Token)
	in.Liquidity, in.RiskFlagged = e.tokenMeta(ctx, tok.Address)
	in.AgentDrawdownPct = leaderboard.CurrentDrawdown(e.Store.PerfEquity(in.AgentID))
	if err := normalizeStyle(&in); err != nil {
		return nil, deny("ORDER_TYPE", err.Error()), nil
	}
	chk := in
	if in.EntryLimit > 0 {
		chk.Price = in.EntryLimit
	}
	if d := e.Referee.Check(chk, usdc); !d.OK {
		return nil, d, nil
	}
	ref := in.Price
	if _, fresh := e.tokenHistory(tok.Address, 3*e.Cfg.PriceEvery); fresh {
		ref = e.tokenPrice(tok.Address)
	}
	opts := BuyOpts{External: true, Style: in.Style, MaxSlippage: in.MaxSlippage, NoNetTargets: true}
	if foreign {
		ct := ch.ETH
		opts.Token, opts.Chain = &ct, &ch
	} else if !strings.EqualFold(tok.Address, e.Cfg.WETH) {
		opts.Token = &tok
	}
	if in.Style == "twap" {
		opts.TwapSeconds, opts.TwapBuckets = in.TwapMinutes*60, min(max(in.TwapMinutes, 2), 2560)
	}
	p, err := e.Exec.Buy(ctx, w, in.SizeUSD, in.TakeProfit, in.StopLoss, in.EntryLimit, ref, opts)
	if err != nil {
		return nil, referee.Decision{OK: true}, err
	}
	selfMu.Lock()
	for id, s := range selfQue {
		if time.Since(s.at) > 10*time.Minute {
			delete(selfQue, id)
		}
	}
	selfQue[p.External.QuoteID] = selfPending{in: in, w: w, d: referee.Decision{OK: true}, usdc: usdc, ref: ref, tok: tok, at: time.Now()}
	selfMu.Unlock()
	return p, referee.Decision{OK: true}, nil
}

// SelfSubmit sends a wallet-signed order and books it like any arena trade.
func (e *Engine) SelfSubmit(ctx context.Context, quoteID, addr, entrySig, bracketSig string) (Result, error) {
	selfMu.Lock()
	sp, ok := selfQue[quoteID]
	delete(selfQue, quoteID)
	selfMu.Unlock()
	if !ok {
		return Result{}, errors.New("quote not found or already used; ask for a new one")
	}
	if !strings.EqualFold(sp.w.Address(), addr) {
		return Result{}, errors.New("this quote belongs to another wallet")
	}
	if on, why := e.Halted(); on {
		return Result{Decision: deny("HALTED", "emergency stop is on ("+why+")")}, nil
	}
	p, err := e.Exec.SubmitExternal(ctx, quoteID, sp.w.Address(), entrySig, bracketSig)
	if err != nil {
		return Result{Decision: sp.d, Error: err.Error()}, nil
	}
	return e.record(ctx, sp.in, sp.w, sp.d, p, sp.usdc, sp.ref, sp.tok), nil
}

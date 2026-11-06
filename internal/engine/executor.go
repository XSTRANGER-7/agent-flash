package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// Executor turns an approved trade into a Flash order:
// quote -> guards (price, cost, typed data, setup txs) -> capped approve ->
// sign -> submit. Nothing onchain happens before every guard has passed.
type Executor struct {
	cfg   *config.Config
	flash *flash.Client
	rpc   *evm.RPC
	log   *slog.Logger

	mu          sync.Mutex
	walletLocks map[string]*sync.Mutex // one in-flight order per wallet (nonce + allowance safety)
	settlement  *pinSet                // FlashOrder verifyingContract == approve spender
	vaults      *pinSet                // FlashOrder.message.vault
}

// pinSet is an allowlist, or trust-on-first-use when the allowlist is empty:
// the first address seen is pinned and every other address is refused.
type pinSet struct {
	name    string
	mu      sync.Mutex
	allowed map[string]bool
	fixed   bool // from config
	log     *slog.Logger
}

func newPinSet(name string, list []string, log *slog.Logger) *pinSet {
	p := &pinSet{name: name, allowed: map[string]bool{}, fixed: len(list) > 0, log: log}
	for _, a := range list {
		p.allowed[strings.ToLower(strings.TrimSpace(a))] = true
	}
	return p
}

func (p *pinSet) check(addr string) error {
	a := strings.ToLower(strings.TrimSpace(addr))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.allowed[a] {
		return nil
	}
	if p.fixed {
		return fmt.Errorf("%s %s is not on the allowlist; refusing to sign", p.name, addr)
	}
	if len(p.allowed) == 0 {
		p.allowed[a] = true
		p.log.Warn("trust-on-first-use: pinned "+p.name+"; put it in .env to make this permanent", "address", addr)
		return nil
	}
	return fmt.Errorf("%s changed to %s (pinned a different one); refusing to sign", p.name, addr)
}

func (p *pinSet) has(addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allowed[strings.ToLower(strings.TrimSpace(addr))]
}

func (p *pinSet) List() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.allowed))
	for a := range p.allowed {
		out = append(out, a)
	}
	return out
}

func NewExecutor(cfg *config.Config, fc *flash.Client, rpc *evm.RPC, log *slog.Logger) *Executor {
	return &Executor{cfg: cfg, flash: fc, rpc: rpc, log: log, walletLocks: map[string]*sync.Mutex{},
		settlement: newPinSet("Flash settlement contract", cfg.SettlementAllowlist, log),
		vaults:     newPinSet("Flash vault", cfg.VaultAllowlist, log)}
}

// Settlement returns the pinned settlement contract(s).
func (e *Executor) Settlement() []string { return e.settlement.List() }

func (e *Executor) lock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.walletLocks[id]
	if !ok {
		m = &sync.Mutex{}
		e.walletLocks[id] = m
	}
	return m
}

// Placement is what the executor reports back.
type Placement struct {
	OrderID             string       `json:"orderId"`
	QuoteID             string       `json:"quoteId"`
	DryRun              bool         `json:"dryRun"`
	Paper               bool         `json:"paper,omitempty"`
	SpentUSD            float64      `json:"spentUsd"`
	ApproveTx           string       `json:"approveTx,omitempty"`
	QuotedPrice         float64      `json:"quotedPrice,omitempty"` // all-in: spent / received
	MarkPrice           float64      `json:"markPrice,omitempty"`   // Flash valuation of WETH
	FeeUSD              float64      `json:"feeUsd,omitempty"`
	CostUSD             float64      `json:"costUsd,omitempty"` // from.notional - to.notional
	CostPct             float64      `json:"costPct,omitempty"`
	TakeProfit          float64      `json:"takeProfit,omitempty"`
	StopLoss            float64      `json:"stopLoss,omitempty"`
	OriginalTP          float64      `json:"originalTp,omitempty"`
	RecommendedSlippage float64      `json:"recommendedSlippage,omitempty"`
	Steps               []store.Step `json:"steps"`
	// External is set for self-custody orders: what the person's wallet
	// must sign (and approve) before SubmitExternal.
	External *ExternalOrder `json:"external,omitempty"`
}

func (p *Placement) step(name, detail string) {
	p.Steps = append(p.Steps, store.Step{At: time.Now().UTC(), Step: name, Detail: detail})
}

var (
	// ErrPriceGuard: Flash's price is too far from the onchain reference.
	ErrPriceGuard = errors.New("price guard")
	// ErrCostGuard: the all-in cost of entering is too high for this size.
	ErrCostGuard = errors.New("cost guard")
)

// quoteCosts reads the economics of a quote.
//   - mark: Flash's USD valuation of WETH (notional / amount of the WETH leg)
//   - allIn: USDC per WETH actually paid or received after every cost
//   - cost: from.notional - to.notional (gas is built into market quotes)
func quoteCosts(q *flash.QuoteResponse, buy bool) (mark, allIn, costUSD, costPct float64, ok bool) {
	if q.From == nil || q.To == nil {
		return 0, 0, 0, 0, false
	}
	fa, fn := q.From.Amount.Float(), q.From.Notional.Float()
	ta, tn := q.To.Amount.Float(), q.To.Notional.Float()
	if fa <= 0 || ta <= 0 || fn <= 0 || tn <= 0 {
		return 0, 0, 0, 0, false
	}
	if buy { // from = USDC, to = WETH
		mark, allIn = tn/ta, fa/ta
	} else { // from = WETH, to = USDC
		mark, allIn = fn/fa, ta/fa
	}
	costUSD = fn - tn
	return mark, allIn, costUSD, costUSD / fn * 100, true
}

func (e *Executor) checkPrice(mark, ref float64) error {
	if ref <= 0 || e.cfg.MaxPriceDeviationPct <= 0 {
		return nil
	}
	if dev := math.Abs(mark-ref) / ref * 100; dev > e.cfg.MaxPriceDeviationPct {
		metrics.Inc("price_guard_blocks_total")
		return fmt.Errorf("%w: Flash values WETH at $%.2f vs onchain $%.2f (%.2f%% > %.2f%%)", ErrPriceGuard, mark, ref, dev, e.cfg.MaxPriceDeviationPct)
	}
	return nil
}

// checkQuoteShape refuses anything we do not sign or cannot handle.
func checkQuoteShape(q *flash.QuoteResponse, wantBracket bool) error {
	switch {
	case q.EVM == nil || q.EVM.OrderTypedData == "":
		return errors.New("quote returned no EVM signing payload (is funderAddress set?)")
	case q.EVM.PermitTypedData != "":
		return errors.New("quote asked for a Permit2 signature; refused by policy (direct capped approvals only)")
	case q.NeedsWrap():
		return errors.New("quote asks for a native-asset wrap; not expected for USDC/WETH, refusing")
	case q.BridgeQuoteID != nil && *q.BridgeQuoteID != "":
		return errors.New("quote is crosschain (bridgeQuoteId set); only same-chain Base orders are allowed")
	}
	if wantBracket {
		ab := q.AttachedBracket
		if ab == nil || ab.EVM == nil || ab.EVM.OrderTypedData == "" {
			return errors.New("quote did not return a bracket payload; refusing a naked entry")
		}
		if ab.EVM.PermitTypedData != "" {
			return errors.New("bracket asked for a Permit2 signature; refused by policy")
		}
		if ab.Deadline == "" || ab.SignedMaxFromAmount == "" {
			return errors.New("bracket payload is missing deadline or signedMaxFromAmount")
		}
	} else if q.AttachedBracket != nil {
		return errors.New("quote returned an unexpected bracket")
	}
	return nil
}

func (e *Executor) buyRequest(w *wallet.Wallet, usd, tp, sl, entryLimit float64) *flash.QuoteRequest {
	return e.buyRequestFor(w, e.token(nil), usd, tp, sl, entryLimit)
}

// token resolves an order token (nil = the agents' asset, WETH).
func (e *Executor) token(t *config.Token) config.Token {
	if t != nil && t.Address != "" {
		return *t
	}
	return config.Token{Symbol: e.cfg.TradeAsset, Address: e.cfg.WETH, Decimals: 18}
}

func (e *Executor) buyRequestFor(w *wallet.Wallet, tok config.Token, usd, tp, sl, entryLimit float64) *flash.QuoteRequest {
	noPermit2 := false
	req := &flash.QuoteRequest{
		TargetChain: e.cfg.Chain, ContraChain: e.cfg.Chain,
		TargetAsset: tok.Address, ContraAsset: e.cfg.USDC,
		Side: "buy", Qty: fmtUSD(usd), OrderType: "market",
		FunderAddress:         w.Address(),
		MaxSlippage:           e.cfg.MaxSlippage,
		FlashIntegratorFeeBps: e.cfg.FeeBps,
		ForceMinimalAllowance: true,       // Lock 2: never ask for unlimited allowance
		EVMUsePermit2:         &noPermit2, // direct, capped ERC-20 approval only
		AttachedBracket: &flash.QuoteBracket{
			TakeProfit: &flash.BracketLeg{NotionalPrice: fmtPrice(tp)},
			StopLoss:   &flash.BracketLeg{NotionalPrice: fmtPrice(sl)},
			ExpireTime: time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	if entryLimit > 0 {
		// Limit entry + bracket: docs require limitCrossPrice with an attached bracket.
		req.OrderType = "limit"
		req.MaxSlippage = "" // maxSlippage is a market-order control
		req.LimitCrossPrice = fmtPrice(entryLimit)
		req.ExpireTime = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return req
}

// BuyWithBracket buys `usd` of WETH with USDC and attaches a take-profit /
// stop-loss pair. With NetTargets the take profit is raised by the estimated
// round-trip cost (twice the entry cost), so a "win" is a win after costs.
// BuyOpts change the guards for one order (operator test trades).
type BuyOpts struct {
	Token        *config.Token // nil = WETH
	MaxSlippage  string        // "" = MAX_SLIPPAGE (personal plans choose their own)
	MaxCostPct   float64       // 0 = MAX_ENTRY_COST_PCT
	NoNetTargets bool          // keep the take profit where it was asked
	// Entry style (all through the same Flash /quote + /order endpoints):
	// "" or market, limit (entryLimit), twap (DCA: TwapSeconds split into
	// TwapBuckets slices, bracket attached), stop (stop buy at StopPrice; Flash
	// does not attach brackets to stops, so protection is placed after the fill).
	Style       string
	TwapSeconds int
	TwapBuckets int
	StopPrice   float64
	// External: the order is signed in the person's own wallet. Buy runs
	// every guard, then returns the payloads instead of signing.
	External bool
}

func (e *Executor) BuyWithBracket(ctx context.Context, w *wallet.Wallet, usd, tp, sl, entryLimit, refPrice float64) (*Placement, error) {
	return e.Buy(ctx, w, usd, tp, sl, entryLimit, refPrice, BuyOpts{})
}

// Buy is BuyWithBracket with per-order options.
func (e *Executor) Buy(ctx context.Context, w *wallet.Wallet, usd, tp, sl, entryLimit, refPrice float64, opts BuyOpts) (*Placement, error) {
	l := e.lock(w.ID)
	l.Lock()
	defer l.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	tok := e.token(opts.Token)
	req := e.buyRequestFor(w, tok, usd, tp, sl, entryLimit)
	if opts.MaxSlippage != "" && entryLimit == 0 {
		req.MaxSlippage = opts.MaxSlippage
	}
	switch opts.Style {
	case "twap":
		if opts.TwapSeconds < 300 || opts.TwapBuckets < 2 || opts.TwapBuckets > 2560 {
			return nil, errors.New("TWAP needs at least 5 minutes and 2-2560 slices")
		}
		req.OrderType, req.DurationSeconds, req.TwapBucketCount = "twap", opts.TwapSeconds, opts.TwapBuckets
		req.LimitCrossPrice, req.ExpireTime, req.MaxSlippage = "", "", "" // twap rejects expireTime
	case "stop":
		if opts.StopPrice <= 0 {
			return nil, errors.New("stop buy needs a trigger price")
		}
		req.OrderType = "stop"
		req.Triggers = []flash.Trigger{{NotionalPrice: fmtPrice(opts.StopPrice), TriggerType: "upper"}}
		req.AttachedBracket, req.LimitCrossPrice, req.MaxSlippage = nil, "", ""
		req.ExpireTime = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		opts.NoNetTargets = true
	}
	bracket := req.AttachedBracket != nil
	p := &Placement{DryRun: e.cfg.DryRun, Paper: e.cfg.Paper, SpentUSD: usd, TakeProfit: tp, StopLoss: sl, OriginalTP: tp}

	q, err := e.quote(ctx, req)
	if err != nil {
		return nil, err
	}
	market := req.OrderType != "limit" // market, twap and stop are priced at the current market
	targetsNote := ""
	if market {
		if err := e.marketGuards(q, refPrice, p, opts.MaxCostPct); err != nil {
			return nil, err
		}
		if bracket && e.cfg.NetTargets && !opts.NoNetTargets && p.CostPct > 0 {
			bump := round2(p.MarkPrice * 2 * p.CostPct / 100)
			if bump >= 0.01 {
				newTP := round2(tp + bump)
				req.AttachedBracket.TakeProfit = &flash.BracketLeg{NotionalPrice: fmtPrice(newTP)}
				if q, err = e.quote(ctx, req); err != nil {
					return nil, err
				}
				if err := e.marketGuards(q, refPrice, p, opts.MaxCostPct); err != nil {
					return nil, err
				}
				p.TakeProfit = newTP
				targetsNote = fmt.Sprintf("take profit raised $%.2f -> $%.2f to cover ~%.2f%% round-trip Flash cost", tp, newTP, 2*p.CostPct)
			}
		}
	}
	p.QuoteID, p.FeeUSD = q.QuoteID, q.FeeNotional()
	p.RecommendedSlippage = q.RecommendedSlippage.Float()
	detail := "quote " + q.QuoteID
	if market {
		detail += fmt.Sprintf(" · mark $%.2f · all-in cost $%.3f (%.2f%%)", p.MarkPrice, p.CostUSD, p.CostPct)
		if rs := p.RecommendedSlippage; rs > 0 {
			detail += fmt.Sprintf(" · Flash suggests %.1f%% slippage, we cap at %s", rs*100, pctString(e.cfg.MaxSlippage))
		}
	}
	p.step("quoted", detail)
	if targetsNote != "" {
		p.step("targets", targetsNote)
	}

	// Guards: never blind-sign. Everything is checked before any onchain tx.
	qtyUnits, err := decToUnits(req.Qty, 6)
	if err != nil {
		return nil, err
	}
	if err := e.guardTypedData(q.EVM.OrderTypedData, w.Address(), expect{from: e.cfg.USDC, to: tok.Address, exact: qtyUnits}); err != nil {
		return nil, fmt.Errorf("entry payload: %w", err)
	}
	if q.To == nil || q.To.Amount.Float() <= 0 {
		return nil, errors.New("quote does not say how much the entry buys; refusing")
	}
	if bracket {
		signedMax, err := decToUnits(q.AttachedBracket.SignedMaxFromAmount.String(), tok.Decimals)
		if err != nil {
			return nil, fmt.Errorf("bracket signedMaxFromAmount: %w", err)
		}
		// The pair sells what the entry received, capped at signedMax. Flash
		// signs ~2x for limit and TWAP entries (a lower fill price buys more),
		// ~1.07x for market ones.
		ratio := 1.25
		if req.OrderType == "limit" || req.OrderType == "twap" {
			ratio = 2.1
		}
		if got := q.To.Amount.Float(); q.AttachedBracket.SignedMaxFromAmount.Float() > got*ratio {
			return nil, fmt.Errorf("bracket may sell %s %s but the entry only buys %.10f; refusing", q.AttachedBracket.SignedMaxFromAmount, tok.Symbol, got)
		}
		if err := e.guardTypedData(q.AttachedBracket.EVM.OrderTypedData, w.Address(), expect{from: tok.Address, to: e.cfg.USDC, max: signedMax}); err != nil {
			return nil, fmt.Errorf("bracket payload: %w", err)
		}
	}
	if err := e.checkSetupTxs(q.SetupTxs, w.Address()); err != nil {
		return nil, err
	}

	if opts.External {
		// Self-custody: hand the checked payloads to the person's wallet.
		return e.prepareExternal(w, req, q, bracket, usd, tok, p)
	}

	// Allowances (capped, after the guards).
	if q.EVM.ApproveTx != nil {
		h, err := e.cappedApprove(ctx, w, q.EVM.ApproveTx, e.cfg.USDC, 6, math.Min(usd, e.cfg.MaxApprovalUSD))
		if err != nil {
			return nil, fmt.Errorf("approve USDC: %w", err)
		}
		p.ApproveTx = h
		p.step("approved", fmt.Sprintf("USDC allowance set to $%.2f for the Flash settlement contract (tx %s)", math.Min(usd, e.cfg.MaxApprovalUSD), h))
	}
	if bracket && q.AttachedBracket.EVM.ApproveTx != nil {
		// The bracket later sells the WETH it bought: cap at signedMaxFromAmount (+1%).
		capWETH := q.AttachedBracket.SignedMaxFromAmount.Float() * 1.01
		h, err := e.cappedApprove(ctx, w, q.AttachedBracket.EVM.ApproveTx, tok.Address, tok.Decimals, capWETH)
		if err != nil {
			return nil, fmt.Errorf("approve %s for bracket: %w", tok.Symbol, err)
		}
		p.step("approved", fmt.Sprintf("%s allowance capped at %.8f for the exit (tx %s)", tok.Symbol, capWETH, h))
	}

	entrySig, err := w.Signer.SignTypedData(q.EVM.OrderTypedData)
	if err != nil {
		return nil, fmt.Errorf("sign entry: %w", err)
	}
	order := flash.NewOrderRequest(req, q.QuoteID, entrySig, q.EVM.OrderTypedData)
	order.AttributionCode = e.cfg.AttributionCode
	signedWhat := "entry signed"
	if bracket {
		bSig, err := w.Signer.SignTypedData(q.AttachedBracket.EVM.OrderTypedData)
		if err != nil {
			return nil, fmt.Errorf("sign bracket: %w", err)
		}
		order.AttachedBracket = &flash.SubmitBracket{
			TakeProfit: req.AttachedBracket.TakeProfit, StopLoss: req.AttachedBracket.StopLoss,
			UserSignature: bSig, Deadline: q.AttachedBracket.Deadline,
			SignedMaxFromAmount: q.AttachedBracket.SignedMaxFromAmount.String(), Salt: q.AttachedBracket.Salt,
		}
		signedWhat = "entry + bracket signed"
	}
	p.step("signed", signedWhat+" (EIP-712, FlashOrder checked: funder, tokens, amount, vault, contract)")
	if e.cfg.DryRun {
		p.step("dry_run", "not submitted (DRY_RUN=true)")
		e.log.Info("dry-run: signed but not submitted", "wallet", w.ID, "quoteId", q.QuoteID, "usd", usd)
		p.OrderID = "dryrun-" + q.QuoteID
		return p, nil
	}

	// Submit once (never auto-retried, see flash client).
	res, err := e.flash.SubmitOrder(ctx, order)
	metrics.Inc("flash_orders_total")
	if err != nil {
		metrics.Inc("flash_order_errors_total")
		return nil, fmt.Errorf("submit: %w", err)
	}
	p.OrderID = res.OrderID
	switch req.OrderType {
	case "twap":
		p.step("submitted", fmt.Sprintf("Flash TWAP %s: %d slices over %d min (bracket activates on the first slice)", res.OrderID, req.TwapBucketCount, req.DurationSeconds/60))
	case "stop":
		p.step("submitted", fmt.Sprintf("Flash stop buy %s: buys when price reaches $%s; protection is placed after the fill", res.OrderID, req.Triggers[0].NotionalPrice))
	default:
		p.step("submitted", "Flash order "+res.OrderID+" (bracket activates on the first fill)")
	}
	return p, nil
}

func (e *Executor) quote(ctx context.Context, req *flash.QuoteRequest) (*flash.QuoteResponse, error) {
	q, err := e.flash.Quote(ctx, req)
	metrics.Inc("flash_quotes_total")
	if err != nil {
		metrics.Inc("flash_quote_errors_total")
		return nil, fmt.Errorf("quote: %w", err)
	}
	if err := checkQuoteShape(q, req.AttachedBracket != nil); err != nil {
		return nil, err
	}
	return q, nil
}

func (e *Executor) marketGuards(q *flash.QuoteResponse, ref float64, p *Placement, maxCost float64) error {
	if maxCost <= 0 {
		maxCost = e.cfg.MaxEntryCostPct
	}
	mark, allIn, cost, pct, ok := quoteCosts(q, true)
	if !ok {
		return errors.New("quote has no from/to notionals; cannot verify the price, refusing")
	}
	p.MarkPrice, p.QuotedPrice, p.CostUSD, p.CostPct = mark, allIn, cost, pct
	if err := e.checkPrice(mark, ref); err != nil {
		return err
	}
	if maxCost > 0 && pct > maxCost {
		metrics.Inc("cost_guard_blocks_total")
		return fmt.Errorf("%w: entering $%.2f costs $%.3f (%.2f%%, limit %.2f%%); trade a larger size", ErrCostGuard, p.SpentUSD, cost, pct, maxCost)
	}
	return nil
}

// SellMarket sells `weth` WETH for USDC at market (operator "exit now").
func (e *Executor) SellMarket(ctx context.Context, w *wallet.Wallet, weth, refPrice float64) (*Placement, error) {
	return e.SellToken(ctx, w, e.token(nil), weth, refPrice)
}

// SellToken sells `amount` of tok for USDC at market.
func (e *Executor) SellToken(ctx context.Context, w *wallet.Wallet, tok config.Token, amount, refPrice float64) (*Placement, error) {
	weth := amount
	l := e.lock(w.ID)
	l.Lock()
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	noPermit2 := false
	qty := fmtUnits(weth, tok.Decimals)
	req := &flash.QuoteRequest{
		TargetChain: e.cfg.Chain, ContraChain: e.cfg.Chain, TargetAsset: tok.Address, ContraAsset: e.cfg.USDC,
		Side: "sell", Qty: qty, OrderType: "market", FunderAddress: w.Address(), MaxSlippage: e.cfg.MaxSlippage,
		FlashIntegratorFeeBps: e.cfg.FeeBps, ForceMinimalAllowance: true, EVMUsePermit2: &noPermit2,
	}
	q, err := e.quote(ctx, req)
	if err != nil {
		return nil, err
	}
	mark, allIn, cost, pct, ok := quoteCosts(q, false)
	if !ok {
		return nil, errors.New("sell quote has no notionals; refusing")
	}
	if err := e.checkPrice(mark, refPrice); err != nil {
		return nil, err
	}
	p := &Placement{QuoteID: q.QuoteID, DryRun: e.cfg.DryRun, Paper: e.cfg.Paper, MarkPrice: mark, QuotedPrice: allIn, CostUSD: cost, CostPct: pct, FeeUSD: q.FeeNotional()}
	p.step("exit_quoted", fmt.Sprintf("sell %s "+tok.Symbol+" · mark $%.2f · receive ~$%.2f (cost $%.3f)", qty, mark, q.To.Amount.Float(), cost))
	units, err := decToUnits(qty, tok.Decimals)
	if err != nil {
		return nil, err
	}
	if err := e.guardTypedData(q.EVM.OrderTypedData, w.Address(), expect{from: tok.Address, to: e.cfg.USDC, exact: units}); err != nil {
		return nil, fmt.Errorf("exit payload: %w", err)
	}
	if err := e.checkSetupTxs(q.SetupTxs, w.Address()); err != nil {
		return nil, err
	}
	if q.EVM.ApproveTx != nil {
		if _, err := e.cappedApprove(ctx, w, q.EVM.ApproveTx, tok.Address, tok.Decimals, weth*1.01); err != nil {
			return nil, fmt.Errorf("approve %s: %w", tok.Symbol, err)
		}
	}
	sig, err := w.Signer.SignTypedData(q.EVM.OrderTypedData)
	if err != nil {
		return nil, err
	}
	if e.cfg.DryRun {
		p.OrderID = "dryrun-" + q.QuoteID
		return p, nil
	}
	order := flash.NewOrderRequest(req, q.QuoteID, sig, q.EVM.OrderTypedData)
	order.AttributionCode = e.cfg.AttributionCode
	res, err := e.flash.SubmitOrder(ctx, order)
	metrics.Inc("flash_orders_total")
	if err != nil {
		metrics.Inc("flash_order_errors_total")
		return nil, fmt.Errorf("submit exit: %w", err)
	}
	p.OrderID = res.OrderID
	p.step("exit_submitted", "Flash market sell "+res.OrderID)
	return p, nil
}

// MoveStop re-prices a live bracket (PATCH /orders/{id}), keeping take profit.
func (e *Executor) MoveStop(ctx context.Context, w *wallet.Wallet, bracketOrderID string, newSL, tp float64) error {
	if e.cfg.DryRun {
		return errors.New("dry run: nothing to update")
	}
	sl, tps := fmtPrice(newSL), fmtPrice(tp)
	msg := flash.BracketUpdateMessage(bracketOrderID, time.Now(), sl, tps)
	sig, err := w.Signer.SignPersonal(msg)
	if err != nil {
		return err
	}
	return e.flash.UpdateBracket(ctx, bracketOrderID, sl, tps, msg, sig)
}

// Cancel signs the canonical cancel message (EIP-191) and calls Flash.
func (e *Executor) Cancel(ctx context.Context, w *wallet.Wallet, orderID string) error {
	if strings.HasPrefix(orderID, "dryrun-") {
		return nil
	}
	sig, err := w.Signer.SignPersonal(flash.CancelMessage(orderID))
	if err != nil {
		return err
	}
	return e.flash.Cancel(ctx, orderID, sig)
}

// expect is what a FlashOrder payload must say before we sign it.
type expect struct {
	from, to string
	exact    *big.Int // fromAmount must equal this (±1 unit), or
	max      *big.Int // fromAmount must not exceed this (+1 unit)
}

// flashOrderType is the FlashOrder struct as Flash returns it (checked
// field by field, because the types define what the signature means).
var flashOrderType = []evm.TypedField{
	{Name: "swapper", Type: "address"}, {Name: "vault", Type: "address"}, {Name: "recipient", Type: "address"},
	{Name: "fromToken", Type: "address"}, {Name: "toToken", Type: "address"}, {Name: "fromAmount", Type: "uint256"},
	{Name: "salt", Type: "uint256"}, {Name: "deadline", Type: "uint256"},
}

// guardTypedData refuses any payload that is not exactly the order we mean:
// Base chain id, pinned settlement contract, FlashOrder shape, our wallet as
// swapper and recipient, the right tokens and amount, a pinned vault and a
// deadline in the future.
func (e *Executor) guardTypedData(raw, funder string, x expect) error {
	td, err := evm.ParseTypedData(raw)
	if err != nil {
		return err
	}
	cid, ok := td.Domain["chainId"]
	if !ok {
		return errors.New("typed data has no chainId; refusing to sign")
	}
	if n, err := evm.ToBigInt(cid); err != nil || n.Int64() != e.cfg.ChainID {
		return fmt.Errorf("typed data chainId %v != %d", cid, e.cfg.ChainID)
	}
	vc, _ := td.Domain["verifyingContract"].(string)
	if vc == "" {
		return errors.New("typed data has no verifyingContract; refusing to sign")
	}
	if err := e.settlement.check(vc); err != nil {
		return err
	}
	if td.PrimaryType != "FlashOrder" {
		return fmt.Errorf("unknown Flash payload type %q; refusing to sign", td.PrimaryType)
	}
	fields := td.Types["FlashOrder"]
	if len(fields) != len(flashOrderType) {
		return errors.New("FlashOrder type changed; refusing to sign")
	}
	for i, f := range fields {
		if f != flashOrderType[i] {
			return fmt.Errorf("FlashOrder field %d is %s %s, expected %s %s; refusing", i, f.Type, f.Name, flashOrderType[i].Type, flashOrderType[i].Name)
		}
	}
	m := td.Message
	str := func(k string) string { s, _ := m[k].(string); return s }
	switch {
	case !strings.EqualFold(str("swapper"), funder):
		return errors.New("typed data swapper is not our wallet; refusing to sign")
	case !strings.EqualFold(str("recipient"), funder):
		return errors.New("typed data sends the proceeds to another address; refusing to sign")
	case !strings.EqualFold(str("fromToken"), x.from):
		return fmt.Errorf("typed data spends %s, expected %s", str("fromToken"), x.from)
	case !strings.EqualFold(str("toToken"), x.to):
		return fmt.Errorf("typed data buys %s, expected %s", str("toToken"), x.to)
	}
	if err := e.vaults.check(str("vault")); err != nil {
		return err
	}
	amt, err := evm.ToBigInt(m["fromAmount"])
	if err != nil {
		return fmt.Errorf("typed data fromAmount: %w", err)
	}
	one := big.NewInt(1)
	if x.exact != nil {
		if d := new(big.Int).Sub(amt, x.exact); d.CmpAbs(one) > 0 {
			return fmt.Errorf("typed data spends %s units, expected %s", amt, x.exact)
		}
	}
	if x.max != nil && amt.Cmp(new(big.Int).Add(x.max, one)) > 0 {
		return fmt.Errorf("typed data may spend %s units, more than the %s signed maximum", amt, x.max)
	}
	dl, err := evm.ToBigInt(m["deadline"])
	if err != nil || dl.Int64() <= time.Now().Unix() {
		return fmt.Errorf("typed data deadline %v is missing or in the past", m["deadline"])
	}
	return nil
}

// checkSetupTxs inspects Flash's unsigned setup transactions. We never sign
// them (we send our own capped approve instead), but we only proceed when
// they are plain approvals to the pinned settlement contract on our tokens,
// i.e. exactly what our own approve already covers.
func (e *Executor) checkSetupTxs(txs []string, funder string) error {
	for i, raw := range txs {
		b, err := evm.DecodeHex(raw)
		if err != nil {
			return fmt.Errorf("setup tx %d: %w", i, err)
		}
		tx, err := evm.DecodeDynamicFeeTx(b)
		if err != nil {
			return fmt.Errorf("setup tx %d: %w; refusing (unknown onchain prep)", i, err)
		}
		if tx.ChainID.Int64() != e.cfg.ChainID {
			return fmt.Errorf("setup tx %d is for chain %s", i, tx.ChainID)
		}
		if tx.Value.Sign() != 0 || !(strings.EqualFold(tx.To, e.cfg.USDC) || e.isToken(tx.To)) {
			return fmt.Errorf("setup tx %d is not a USDC/WETH approval (to %s); refusing", i, tx.To)
		}
		spender, _, ok := evm.ParseApprove(tx.Data)
		if !ok || !e.settlement.has(spender) {
			return fmt.Errorf("setup tx %d is not an approve() to the pinned settlement contract; refusing", i)
		}
	}
	if len(txs) > 0 {
		e.log.Info("flash offered setup txs; covered by our own capped approve", "n", len(txs), "funder", funder)
	}
	return nil
}

// cappedApprove re-encodes Flash's approve calldata with min(requested, cap)
// and sends it (Lock 2, the onchain spending limit). The spender must be the
// pinned Flash settlement contract.
func (e *Executor) cappedApprove(ctx context.Context, w *wallet.Wallet, tx *flash.EVMTx, token string, decimals int, capUnits float64) (string, error) {
	if !strings.EqualFold(tx.To, token) {
		return "", fmt.Errorf("approve tx targets %s, expected token %s", tx.To, token)
	}
	if tx.Value != "" && tx.Value != "0" && tx.Value != "0x0" {
		return "", errors.New("approve tx carries ETH value; refusing")
	}
	data, err := evm.DecodeHex(tx.Data)
	if err != nil {
		return "", err
	}
	spender, amount, ok := evm.ParseApprove(data)
	if !ok {
		return "", errors.New("approve tx is not an ERC-20 approve(); refusing")
	}
	if !e.settlement.has(spender) {
		return "", fmt.Errorf("approve spender %s is not the pinned Flash settlement contract; refusing", spender)
	}
	capInt := toUnits(capUnits, decimals)
	if amount.Cmp(capInt) > 0 {
		amount = capInt
	}
	newData, err := evm.EncodeApprove(spender, amount)
	if err != nil {
		return "", err
	}
	if e.cfg.Paper {
		e.log.Info("paper: capped approve not broadcast", "wallet", w.ID, "token", token, "amount", amount.String())
		return "paper", nil
	}
	if e.cfg.DryRun {
		e.log.Info("dry-run: would send capped approve", "wallet", w.ID, "token", token, "spender", spender, "amount", amount.String())
		return "dryrun", nil
	}
	key := w.Signer.Key()
	if key == nil {
		return "", errors.New("remote signer cannot send approve txs from this service")
	}
	h, err := e.rpc.SendTx(ctx, big.NewInt(e.cfg.ChainID), key, token, newData, nil)
	if err != nil {
		return h, err
	}
	e.log.Info("capped approve mined", "wallet", w.ID, "token", token, "spender", spender, "amount", amount.String(), "tx", h)
	return h, nil
}

func toUnits(x float64, decimals int) *big.Int {
	f := new(big.Float).SetFloat64(x)
	f.Mul(f, new(big.Float).SetFloat64(math.Pow10(decimals)))
	i, _ := f.Int(nil)
	return i
}

// decToUnits converts an exact decimal string ("2.0000", "0.00094") into
// base units without float rounding. Extra digits beyond `decimals` must be 0.
func decToUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > decimals {
		if strings.Trim(frac[decimals:], "0") != "" {
			return nil, fmt.Errorf("amount %q has more than %d decimals", s, decimals)
		}
		frac = frac[:decimals]
	}
	frac += strings.Repeat("0", decimals-len(frac))
	n, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	return n, nil
}

// fmtUSD formats a USDC amount (6 decimals max).
func fmtUSD(x float64) string {
	return strconv.FormatFloat(math.Floor(x*1e6)/1e6, 'f', -1, 64)
}

// fmtPrice formats a USD price with 2 decimals (byte-identical everywhere).
func fmtPrice(x float64) string { return strconv.FormatFloat(x, 'f', 2, 64) }

// fmtUnits floors to min(decimals, 12) places so we never sell more than we hold.
func fmtUnits(x float64, decimals int) string {
	p := min(decimals, 12)
	m := math.Pow10(p)
	return strconv.FormatFloat(math.Floor(x*m)/m, 'f', p, 64)
}

// fmtWETH floors to 12 decimals so we never try to sell more than we hold.
func fmtWETH(x float64) string {
	return strconv.FormatFloat(math.Floor(x*1e12)/1e12, 'f', -1, 64)
}

func pctString(frac string) string {
	f, _ := strconv.ParseFloat(frac, 64)
	return strconv.FormatFloat(f*100, 'f', -1, 64) + "%"
}

var balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)

// TokenUnits reads an ERC-20 balance onchain in base units (exact).
func (e *Executor) TokenUnits(ctx context.Context, token, owner string) (*big.Int, error) {
	a, err := evm.HexToAddressBytes(owner)
	if err != nil {
		return nil, err
	}
	data := append(append([]byte{}, balanceOfSelector...), make([]byte, 12)...)
	data = append(data, a...)
	out, err := e.rpc.Call(ctx, token, data)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

// Allowance reads how much the pinned settlement contract may pull from owner.
func (e *Executor) Allowance(ctx context.Context, token, owner string) (*big.Int, error) {
	sp := e.settlement.List()
	if len(sp) == 0 {
		return nil, errors.New("no settlement contract pinned yet")
	}
	data, err := evm.EncodeAllowance(owner, sp[0])
	if err != nil {
		return nil, err
	}
	out, err := e.rpc.Call(ctx, token, data)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

// Transfer sends an ERC-20 transfer from a sub-wallet (live mode only; the
// caller checks the destination is the wallet's payout address).
func (e *Executor) Transfer(ctx context.Context, w *wallet.Wallet, token, to string, units *big.Int) (string, error) {
	if !e.cfg.Live() {
		return "", errors.New("transfers are only sent in live mode")
	}
	key := w.Signer.Key()
	if key == nil {
		return "", errors.New("remote signer cannot send transfers from this service")
	}
	data, err := evm.EncodeTransfer(to, units)
	if err != nil {
		return "", err
	}
	l := e.lock(w.ID)
	l.Lock()
	defer l.Unlock()
	return e.rpc.SendTx(ctx, big.NewInt(e.cfg.ChainID), key, token, data, nil)
}

// Revoke sets the settlement contract's allowance on token to zero (live).
// Cancelling Flash orders does not revoke approvals; this does, onchain.
func (e *Executor) Revoke(ctx context.Context, w *wallet.Wallet, token string) (string, error) {
	if !e.cfg.Live() {
		return "", errors.New("revoke is only sent in live mode")
	}
	key := w.Signer.Key()
	if key == nil {
		return "", errors.New("remote signer cannot send txs from this service")
	}
	sp := e.settlement.List()
	if len(sp) == 0 {
		return "", errors.New("no settlement contract pinned")
	}
	data, err := evm.EncodeApprove(sp[0], big.NewInt(0))
	if err != nil {
		return "", err
	}
	l := e.lock(w.ID)
	l.Lock()
	defer l.Unlock()
	return e.rpc.SendTx(ctx, big.NewInt(e.cfg.ChainID), key, token, data, nil)
}

// ETHBalance reads the wallet's native ETH (gas for approvals / transfers).
func (e *Executor) ETHBalance(ctx context.Context, addr string) (float64, error) {
	b, err := e.rpc.ETHBalance(ctx, addr)
	if err != nil {
		return 0, err
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(b), big.NewFloat(1e18)).Float64()
	return f, nil
}

// CancelBatch cancels up to 25 of one wallet's orders with one EIP-191
// signature over Flash's batch message (POST /orders/cancel).
func (e *Executor) CancelBatch(ctx context.Context, w *wallet.Wallet, orderIDs []string) ([]flash.BatchCancelResult, error) {
	if e.cfg.DryRun || len(orderIDs) == 0 {
		return nil, nil
	}
	sig, err := w.Signer.SignPersonal(flash.BatchCancelMessage(orderIDs))
	if err != nil {
		return nil, err
	}
	return e.flash.CancelBatch(ctx, orderIDs, sig)
}

func (e *Executor) isToken(addr string) bool {
	if strings.EqualFold(addr, e.cfg.WETH) {
		return true
	}
	_, ok := e.cfg.TokenByAddress(addr)
	return ok
}

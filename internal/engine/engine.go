// Package engine is the arena's core pipeline:
//
//	agent signal -> intent -> referee (Lock 1) -> [human approval] ->
//	executor (capped approval = Lock 2, sign, Flash) -> store + feed + Arkiv
//	-> copy engine fan-out to followers (same pipeline, in parallel)
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/approval"
	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/leaderboard"
	"github.com/vikram/agent-arena/internal/metrics"
	"github.com/vikram/agent-arena/internal/outbox"
	"github.com/vikram/agent-arena/internal/paper"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

type Engine struct {
	Cfg       *config.Config
	Log       *slog.Logger
	Flash     *flash.Client
	Exec      *Executor
	Referee   *referee.Referee
	Approvals *approval.Manager
	// Ctx is the process lifetime context (cancelled on SIGTERM).
	Ctx     context.Context
	Store   *store.Store
	Feed    *feed.Feed
	Outbox  *outbox.Outbox
	Wallets *wallet.Registry
	Prices  *arena.PriceFeed
	Agents  map[string]arena.Profile
	// Stream is the Flash orders WebSocket (nil = REST polling only).
	Stream *flash.Stream
	// TokenFeeds prices coins other than WETH (lower-case address -> feed).
	TokenFeeds map[string]*arena.QuoteFeed
	// Venue is the paper exchange in MODE=paper (nil otherwise).
	Venue *paper.Venue

	haltMu     sync.Mutex
	halted     bool
	haltReason string

	balMu sync.Mutex
	bals  map[string]cachedBal
	wg    sync.WaitGroup

	refresh     chan string // order ids to re-read now (from the stream)
	refreshOnce sync.Once
	mu          sync.Mutex
	stopTries   map[string]int
	lastBlock   map[string]time.Time // wallet|code -> last feed post (see blockQuiet)
}

// blockQuiet is how long the same referee block (same wallet, same code) stays
// out of the feed after it was posted once. Agents re-check every AGENT_EVERY,
// so an unfunded wallet would otherwise post the same line every few minutes.
const blockQuiet = time.Hour

// shouldPostBlock reports whether this block is news. A different code, or the
// same one after blockQuiet, is posted; repeats are only counted in metrics.
func (e *Engine) shouldPostBlock(walletID, code string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastBlock == nil {
		e.lastBlock = map[string]time.Time{}
	}
	k := walletID + "|" + code
	if at, ok := e.lastBlock[k]; ok && now.Sub(at) < blockQuiet {
		return false
	}
	e.lastBlock[k] = now
	return true
}

// clearBlocks forgets a wallet's posted blocks (it just traded), so the next
// block for it is shown right away.
func (e *Engine) clearBlocks(walletID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.lastBlock {
		if strings.HasPrefix(k, walletID+"|") {
			delete(e.lastBlock, k)
		}
	}
}

type cachedBal struct {
	usdc, weth float64
	all        []flash.Balance
	at         time.Time
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Balances returns (USDC, WETH) with a short cache to respect rate limits.
func (e *Engine) Balances(ctx context.Context, w *wallet.Wallet, maxAge time.Duration) (float64, float64, error) {
	e.balMu.Lock()
	c, ok := e.bals[w.ID]
	e.balMu.Unlock()
	if ok && time.Since(c.at) < maxAge {
		return c.usdc, c.weth, nil
	}
	bs, err := e.Flash.Balances(ctx, w.Address())
	if err != nil {
		return 0, 0, err
	}
	c = cachedBal{usdc: flash.BalanceOf(bs, e.Cfg.Chain, e.Cfg.USDC), weth: flash.BalanceOf(bs, e.Cfg.Chain, e.Cfg.WETH), all: bs, at: time.Now()}
	e.balMu.Lock()
	if e.bals == nil {
		e.bals = map[string]cachedBal{}
	}
	e.bals[w.ID] = c
	e.balMu.Unlock()
	return c.usdc, c.weth, nil
}

func (e *Engine) invalidate(walletID string) {
	e.balMu.Lock()
	delete(e.bals, walletID)
	e.balMu.Unlock()
}

// Result of a proposal, returned to agents and the admin API.
type Result struct {
	Decision referee.Decision `json:"decision"`
	Trade    *store.Trade     `json:"trade,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// Propose is the single entry point for every trade (agent or copy).
func (e *Engine) Propose(ctx context.Context, in referee.Intent) Result {
	metrics.Inc("proposals_total")
	w, ok := e.Wallets.Get(in.WalletID)
	if !ok {
		return Result{Decision: referee.Decision{OK: false, Code: "WALLET", Reason: "unknown wallet " + in.WalletID}}
	}
	if d := e.walletGate(w.ID); !d.OK {
		return e.blocked(in, d)
	}
	usdc, _, err := e.Balances(ctx, w, 20*time.Second)
	if err != nil {
		return Result{Error: "balances: " + err.Error()}
	}
	if in.Test {
		in.SizeUSD = e.Cfg.TestTradeUSD
	}
	if in.SizeUSD == 0 {
		in.SizeUSD = math.Floor(usdc*in.SizePct/100*100) / 100
		// Small wallet: a % slice would be eaten by Flash's fixed cost per
		// order, so trade the minimum instead when the wallet holds it. The
		// referee still applies MAX_POSITION_PCT, the cost guard still runs.
		if m := e.Cfg.MinTradeUSD; m > 0 && in.SizeUSD < m && usdc >= m {
			in.SizeUSD = m
		}
	}
	if in.SizeUSD > e.Cfg.MaxTradeUSD {
		in.SizeUSD = e.Cfg.MaxTradeUSD
	}
	if d := e.budgetGate(in.SizeUSD); !d.OK {
		return e.blocked(in, d)
	}
	// Engine-only safety inputs: exit liquidity, Flash risk flag, agent drawdown.
	tok, _ := e.tokenBySymbol(in.Token)
	in.Liquidity, in.RiskFlagged = e.tokenMeta(ctx, tok.Address)
	in.AgentDrawdownPct = leaderboard.CurrentDrawdown(e.Store.PerfEquity(in.AgentID))
	if err := normalizeStyle(&in); err != nil {
		return e.blocked(in, deny("ORDER_TYPE", err.Error()))
	}
	// Targets are judged against where the entry will happen: the limit price
	// for a limit, the trigger for a stop buy, the market otherwise.
	chk := in
	switch {
	case in.Style == "stop":
		chk.Price = in.StopPrice
	case in.EntryLimit > 0:
		chk.Price = in.EntryLimit
	}
	d := e.Referee.Check(chk, usdc)
	if !d.OK {
		return e.blocked(in, d)
	}

	// The price guard compares Flash's quote with the onchain price right now
	// (not the decision-time price, which can be minutes old for copies).
	ref := in.Price
	if _, fresh := e.tokenHistory(tok.Address, 3*e.Cfg.PriceEvery); fresh {
		ref = e.tokenPrice(tok.Address)
	}
	var opts BuyOpts
	if in.Test {
		// A test proves the plumbing, not the strategy: small size costs more
		// (Flash charges ~$0.06 + 0.36%), so it gets its own cost cap and keeps
		// its take profit close.
		opts = BuyOpts{MaxCostPct: e.Cfg.TestMaxCostPct, NoNetTargets: true}
	}
	if tok.Address != "" && !strings.EqualFold(tok.Address, e.Cfg.WETH) {
		opts.Token = &tok
	}
	opts.MaxSlippage = in.MaxSlippage
	if in.PlanID != "" {
		opts.NoNetTargets = true // the owner chose this take profit; keep it exactly
	}
	opts.Style, opts.StopPrice = in.Style, in.StopPrice
	if in.Style == "twap" {
		opts.TwapSeconds, opts.TwapBuckets = in.TwapMinutes*60, min(max(in.TwapMinutes, 2), 2560) // one slice a minute
	}
	p, err := e.Exec.Buy(ctx, w, in.SizeUSD, in.TakeProfit, in.StopLoss, in.EntryLimit, ref, opts)
	if err != nil {
		metrics.Inc("execution_errors_total")
		e.Log.Error("execution failed", "wallet", w.ID, "err", err)
		e.Feed.Publish(feed.Post{Kind: "trade_blocked", AgentID: in.AgentID, Text: fmt.Sprintf("%s: order failed: %s", e.who(in), shortErr(err))})
		return Result{Decision: d, Error: err.Error()}
	}
	return e.record(ctx, in, w, d, p, usdc, ref, tok)
}

// record books a placed order: referee quota, the trade receipt, the feed,
// the Arkiv outbox and (for an agent's own trade) the copy fan-out.
func (e *Engine) record(ctx context.Context, in referee.Intent, w *wallet.Wallet, d referee.Decision, p *Placement, usdc, ref float64, tok config.Token) Result {
	e.Referee.Commit(w.ID)
	e.invalidate(w.ID)
	e.clearBlocks(w.ID)

	t := &store.Trade{
		ID: newID("trd_"), AgentID: in.AgentID, WalletID: w.ID, CopyOf: in.CopyOf, OrderID: p.OrderID,
		Token: in.Token, OrderType: in.OrderType, SizeUSD: in.SizeUSD, Entry: ref,
		TakeProfit: p.TakeProfit, StopLoss: p.StopLoss, Reason: in.Reason,
		Status: "placed", DryRun: p.DryRun, USDCBefore: usdc, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		QuotedPrice: p.QuotedPrice, FeeUSD: p.FeeUSD, Timeline: p.Steps,
		MarkPrice: p.MarkPrice, EntryCostUSD: p.CostUSD, EntryCostPct: p.CostPct, ApproveTx: p.ApproveTx, Protection: "pending",
		Paper: p.Paper, Test: in.Test, Kind: tradeKind(in, w), PlanID: in.PlanID, TokenAddr: tok.Address, TokenDec: tok.Decimals,
		Style: in.Style, StopPrice: in.StopPrice, TwapMinutes: in.TwapMinutes, ProtectMode: in.ProtectMode, TrailPct: in.TrailPct,
	}
	if p.OriginalTP != p.TakeProfit {
		t.OriginalTP = p.OriginalTP
	}
	if p.MarkPrice > 0 {
		t.BreakEvenPrice = round2(p.MarkPrice * (1 + 2*p.CostPct/100)) // refined from the fill
	}
	if p.DryRun {
		t.Status = "dryrun"
		e.Referee.Closed(w.ID) // nothing is open in dry-run
	}
	e.Store.PutTrade(t)
	metrics.Inc("trades_total")

	kind, text := "trade_placed", fmt.Sprintf("%s: LONG %s $%.2f, TP %.2f, SL %.2f. %s", e.name(in.AgentID), in.Token, in.SizeUSD, t.TakeProfit, t.StopLoss, in.Reason)
	switch t.Kind {
	case "copy":
		kind, text = "copy_placed", fmt.Sprintf("%s copied %s: $%.2f, TP %.2f, SL %.2f", w.ID, e.name(in.AgentID), in.SizeUSD, t.TakeProfit, t.StopLoss)
	case "invest":
		kind, text = "copy_placed", fmt.Sprintf("%s traded %s's money: LONG %s $%.2f, TP %.2f, SL %.2f. %s", e.name(in.AgentID), w.ID, in.Token, in.SizeUSD, t.TakeProfit, t.StopLoss, in.Reason)
	case "plan":
		if pl, ok := e.Store.Plan(in.PlanID); ok {
			text = fmt.Sprintf("%s's plan \"%s\": LONG %s $%.2f, TP %s, SL %s. %s", pl.Owner, pl.Name, in.Token, in.SizeUSD, fmtPx(t.TakeProfit), fmtPx(t.StopLoss), in.Reason)
		}
	case "test":
		text = fmt.Sprintf("Test trade from %s: LONG %s $%.2f, TP %.2f, SL %.2f.", w.ID, in.Token, in.SizeUSD, t.TakeProfit, t.StopLoss)
	case "self":
		a := w.Address()
		who := a[:6] + "…" + a[len(a)-4:]
		if in.CopyOf != "" {
			kind, text = "copy_placed", fmt.Sprintf("%s copied %s from their own wallet: $%.2f, TP %s, SL %s", who, e.name(in.AgentID), in.SizeUSD, fmtPx(t.TakeProfit), fmtPx(t.StopLoss))
		} else {
			text = fmt.Sprintf("%s traded from their own wallet: LONG %s $%.2f, TP %s, SL %s", who, in.Token, in.SizeUSD, fmtPx(t.TakeProfit), fmtPx(t.StopLoss))
		}
	}
	if t.Paper {
		text = "[paper] " + text
	}
	if p.CostPct > 0 {
		text += fmt.Sprintf(" Flash all-in cost %.2f%%.", p.CostPct)
	}
	e.Feed.Publish(feed.Post{Kind: kind, AgentID: in.AgentID, TradeID: t.ID, Text: text, Data: map[string]any{"orderId": p.OrderID, "dryRun": p.DryRun}})
	e.Outbox.Publish(outbox.Event{ID: "trade:" + t.ID, Kind: "trade", TTLHours: 24 * 90,
		Attributes: map[string]any{"type": "trade", "season": e.Cfg.SeasonID, "agentid": t.AgentID, "wallet": strings.ToLower(w.Address()), "token": t.Token, "status": t.Status, "copy": boolInt(t.CopyOf != ""), "sizeusd": t.SizeUSD},
		Payload:    t})

	if in.CopyOf == "" && !in.Test && in.Kind != "invest" && w.Role == wallet.RoleAgent {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			// Copies outlive the HTTP request that proposed the trade, but not
			// the process: on shutdown pending approvals expire and release
			// their reserved daily cap before the final save.
			parent := e.Ctx
			if parent == nil {
				parent = context.WithoutCancel(ctx)
			}
			e.fanOut(parent, in, t)
		}()
	}
	return Result{Decision: d, Trade: t}
}

// fanOut copies an agent trade to its followers, in parallel but bounded,
// each through the full referee + approval + executor pipeline.
func (e *Engine) fanOut(ctx context.Context, in referee.Intent, src *store.Trade) {
	e.forFollowers(ctx, in.AgentID, false, func(f store.Follow) { e.copyOne(ctx, in, src, f) })
}

// investAll trades every investor wallet of an agent on its signal, in
// parallel but bounded. Unlike copies, this does not need the agent's own
// trade to have happened (the agent wallet may hold no money at all).
func (e *Engine) investAll(ctx context.Context, in referee.Intent) {
	e.forFollowers(ctx, in.AgentID, true, func(f store.Follow) {
		inv := in
		inv.Kind = "invest"
		e.copyOne(ctx, inv, nil, f)
	})
}

func (e *Engine) forFollowers(ctx context.Context, agentID string, invest bool, fn func(store.Follow)) {
	sem := make(chan struct{}, max(1, e.Cfg.CopyWorkers))
	var wg sync.WaitGroup
	for _, f := range e.Store.Followers(agentID) {
		if (f.Mode == "invest") != invest {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f store.Follow) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(f)
		}(f)
	}
	wg.Wait()
}

// copyOne places one follower trade: a copy of src, or (src == nil) an
// investor wallet traded directly on the agent's signal.
func (e *Engine) copyOne(ctx context.Context, in referee.Intent, src *store.Trade, f store.Follow) {
	fw, ok := e.Wallets.Get(f.FollowerID)
	if !ok {
		return
	}
	usdc, _, err := e.Balances(ctx, fw, 20*time.Second)
	if err != nil {
		e.Log.Warn("copy: balances", "follower", f.FollowerID, "err", err)
		return
	}
	alloc := f.AllocationPct
	if alloc <= 0 {
		alloc = in.SizePct
	}
	usd := math.Floor(usdc*alloc/100*100) / 100
	if f.MaxPerTradeUSD > 0 && usd > f.MaxPerTradeUSD {
		usd = f.MaxPerTradeUSD
	}
	if usd <= 0 {
		if src == nil && usdc <= 0 {
			// Unfunded investor wallet: let the referee say so (posted once an hour).
			ci := in
			ci.WalletID, ci.SizeUSD, ci.SizePct = f.FollowerID, 0, alloc
			e.Propose(ctx, ci)
		}
		return
	}
	verb := "Copy"
	if src == nil {
		verb = "Invest"
	}
	if !e.Store.ReserveCopySpend(f.FollowerID, usd, f.DailyCapUSD) {
		if e.shouldPostBlock(f.FollowerID, "DAILY_CAP", time.Now()) {
			e.Feed.Publish(feed.Post{Kind: "copy_blocked", AgentID: in.AgentID, Text: fmt.Sprintf("%s hit its daily limit ($%.2f)", f.FollowerID, f.DailyCapUSD)})
		}
		return
	}
	if usd > e.Cfg.ApprovalThresholdUSD {
		r, _ := e.Approvals.Await(ctx, f.FollowerID, in.AgentID, usd, fmt.Sprintf("%s %s: buy $%.2f %s, TP %.2f, SL %.2f", verb, e.name(in.AgentID), usd, in.Token, in.TakeProfit, in.StopLoss))
		if r == nil || r.Status != approval.Approved {
			e.Store.ReleaseCopySpend(f.FollowerID, usd)
			status := "expired"
			if r != nil {
				status = string(r.Status)
			}
			metrics.Inc("approvals_denied_total")
			e.Feed.Publish(feed.Post{Kind: "copy_blocked", AgentID: in.AgentID, Text: fmt.Sprintf("%s for %s not placed: approval %s", verb, f.FollowerID, status)})
			return
		}
		metrics.Inc("approvals_granted_total")
		// The source may have exited while we waited for a human. Copying a
		// closed trade would open a position the agent no longer holds.
		if src == nil {
			// invest: nothing to go stale, the referee re-checks price vs levels
		} else if cur, ok := e.Store.Trade(src.ID); ok && cur.Status != "placed" && cur.Status != "filled" && cur.Status != "dryrun" {
			e.Store.ReleaseCopySpend(f.FollowerID, usd)
			e.Feed.Publish(feed.Post{Kind: "copy_blocked", AgentID: in.AgentID, TradeID: src.ID, Text: fmt.Sprintf("Copy for %s skipped: %s's trade already %s", f.FollowerID, e.name(in.AgentID), cur.Status)})
			return
		}
	}
	ci := in
	ci.WalletID, ci.SizeUSD, ci.SizePct = f.FollowerID, usd, alloc
	if src != nil {
		ci.CopyOf = src.ID
	}
	// Copy at today's price with the agent's levels; the referee refuses the
	// copy if the price has already moved past them.
	if _, fresh := e.Prices.Snapshot(3 * e.Cfg.PriceEvery); fresh {
		ci.Price = e.Prices.Last()
	}
	res := e.Propose(ctx, ci)
	if res.Trade == nil {
		e.Store.ReleaseCopySpend(f.FollowerID, usd)
	}
}

func (e *Engine) name(agentID string) string {
	if p, ok := e.Agents[agentID]; ok {
		return p.Name
	}
	return agentID
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RunAgents runs every agent concurrently on its own ticker.
func (e *Engine) RunAgents(ctx context.Context) {
	var wg sync.WaitGroup
	for id, prof := range e.Agents {
		if _, ok := e.Wallets.Get(id); !ok {
			e.Log.Warn("agent has no wallet; skipping", "agent", id)
			continue
		}
		wg.Add(1)
		go func(id string, prof arena.Profile) {
			defer wg.Done()
			t := time.NewTicker(e.Cfg.AgentEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				prices, fresh := e.Prices.Snapshot(3 * e.Cfg.PriceEvery)
				if !fresh || len(prices) == 0 {
					continue
				}
				sig := prof.Strat.Decide(prices)
				if sig == nil {
					continue
				}
				px := prices[len(prices)-1]
				in := referee.Intent{
					AgentID: id, WalletID: id, OrderType: "market+bracket", Token: e.Cfg.TradeAsset,
					SizePct: sig.SizePct, Price: px, TakeProfit: round2(px * (1 + sig.TPPct/100)), StopLoss: round2(px * (1 - sig.SLPct/100)),
					Reason: sig.Reason,
				}
				if e.Cfg.Live() && !e.Cfg.LiveAgents {
					// Real money only moves when a person asks (test trade) until
					// LIVE_AGENTS=true. The signal is still shown, once an hour.
					if e.shouldPostBlock(id, "LIVE_AGENTS_OFF", time.Now()) {
						e.Feed.Publish(feed.Post{Kind: "trade_blocked", AgentID: id, Text: fmt.Sprintf("%s has a signal (%s). Live auto-trading is off, so no real order was placed.", prof.Name, sig.Reason), Data: map[string]any{"code": "LIVE_AGENTS_OFF"}})
					}
					continue
				}
				res := e.Propose(ctx, in)
				if res.Error != "" {
					e.Log.Warn("agent proposal error", "agent", id, "err", res.Error)
				}
				e.investAll(ctx, in)
			}
		}(id, prof)
	}
	wg.Wait()
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// SampleEquity records portfolio value for every wallet (leaderboard input).
func (e *Engine) SampleEquity(ctx context.Context) {
	t := time.NewTicker(e.Cfg.EquityEvery)
	defer t.Stop()
	for {
		px := e.Prices.Last()
		if px > 0 {
			for _, w := range e.Wallets.List("") {
				usdc, _, err := e.Balances(ctx, w, e.Cfg.EquityEvery/2)
				if err != nil {
					continue
				}
				eq := usdc + e.holdingsUSD(w)
				e.detectFlow(w.ID, eq)
				e.Store.AddEquity(w.ID, eq)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PublishProfiles mirrors agent profiles to Arkiv once per season.
func (e *Engine) PublishProfiles() {
	for id, p := range e.Agents {
		w, ok := e.Wallets.Get(id)
		if !ok {
			continue
		}
		p.Address = w.Address()
		b, _ := json.Marshal(p)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		e.Outbox.Publish(outbox.Event{ID: "agent:" + e.Cfg.SeasonID + ":" + id, Kind: "agent", TTLHours: 24 * 60,
			Attributes: map[string]any{"type": "agent", "season": e.Cfg.SeasonID, "agentid": id, "strategy": p.Strategy, "wallet": strings.ToLower(w.Address())},
			Payload:    payload})
	}
}

// Wait blocks until in-flight copy fan-outs finish (graceful shutdown).
func (e *Engine) Wait() { e.wg.Wait() }

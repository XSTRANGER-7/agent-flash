// Package referee enforces the arena rules (Lock 1, offchain). Every trade,
// agent or copy, passes here before anything is signed.
package referee

import (
	"fmt"
	"sync"
	"time"
)

// Intent is what an agent proposes. Agents never see keys.
type Intent struct {
	AgentID    string  `json:"agentId"`
	WalletID   string  `json:"walletId"`  // agent wallet or follower wallet
	OrderType  string  `json:"orderType"` // market+bracket | limit+bracket
	Token      string  `json:"token"`
	SizePct    float64 `json:"sizePct"`    // % of wallet USDC to spend
	SizeUSD    float64 `json:"sizeUsd"`    // resolved by the engine
	EntryLimit float64 `json:"entryLimit"` // 0 = market entry
	TakeProfit float64 `json:"takeProfit"`
	StopLoss   float64 `json:"stopLoss"`
	Price      float64 `json:"price"` // reference price at decision time
	Reason     string  `json:"reason"`
	CopyOf     string  `json:"copyOf,omitempty"` // trade id this copies

	// Advanced Flash order styles (operator ticket, personal plans):
	Style       string  `json:"style,omitempty"`       // market | limit | twap | stop
	StopPrice   float64 `json:"stopPrice,omitempty"`   // stop buy trigger (above price)
	TwapMinutes int     `json:"twapMinutes,omitempty"` // twap / DCA duration (>= 5)
	ProtectMode string  `json:"protect,omitempty"`     // bracket (default) | trailing
	TrailPct    float64 `json:"trailPct,omitempty"`    // trailing stop distance %

	// Set by the engine / server only:
	Test bool   `json:"-"` // operator test trade: TEST_TRADE_USD, test cost cap, no copies
	Kind string `json:"-"` // invest = an investor wallet traded directly on an agent signal
	// Personal plans: the person chose the size, slippage and coin themselves.
	PlanID      string `json:"-"`
	OwnSize     bool   `json:"-"` // skip MAX_POSITION_PCT (the owner sized it on purpose)
	MaxSlippage string `json:"-"` // "" = MAX_SLIPPAGE
	// A community agent's Advanced settings (0 / "" = the arena's):
	MaxCostPct      float64 `json:"-"`
	MinTradeUSD     float64 `json:"-"`
	MaxTradeUSD     float64 `json:"-"`
	MaxTradesPerDay int     `json:"-"`
	NetTargets      string  `json:"-"` // on | off
	BreakEven       string  `json:"-"` // on | off

	// Set by the engine only (never by API callers):
	Liquidity        float64 `json:"-"` // token USD liquidity from Flash /search
	RiskFlagged      bool    `json:"-"` // Flash riskFlagged
	AgentDrawdownPct float64 `json:"-"` // agent's current drawdown from peak equity
}

// dailyLimit is the arena's trades-a-day limit, or an agent's own one.
func dailyLimit(arena, own int) int {
	if own > 0 {
		return own
	}
	return arena
}

type Policy struct {
	AllowedTokens   map[string]bool
	MaxTradeUSD     float64
	MaxPositionPct  float64
	MaxTradesPerDay int
	MaxOpenPerAgent int
	MinTPPct        float64 // TP must be at least this % above price
	MaxSLPct        float64 // SL may not be further than this % below price
	SeasonOpen      func() bool
	MinLiquidityUSD float64
	MaxDrawdownPct  float64 // circuit breaker for an agent and everyone copying it
}

type Decision struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func deny(code, f string, a ...any) Decision {
	return Decision{OK: false, Code: code, Reason: fmt.Sprintf(f, a...)}
}

type Referee struct {
	p     Policy
	mu    sync.Mutex
	day   string
	count map[string]int // walletID -> trades today
	open  map[string]int // walletID -> open positions
}

func New(p Policy) *Referee {
	return &Referee{p: p, count: map[string]int{}, open: map[string]int{}}
}

func (r *Referee) rollDay() {
	d := time.Now().UTC().Format("2006-01-02")
	if d != r.day {
		r.day = d
		r.count = map[string]int{}
	}
}

// Check validates an intent against the policy. It does not reserve quota;
// call Commit after a successful placement.
func (r *Referee) Check(in Intent, walletUSDC float64) Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollDay()
	switch {
	case r.p.SeasonOpen != nil && !r.p.SeasonOpen():
		return deny("SEASON_CLOSED", "season is not active")
	case !protectedTypes[in.OrderType]:
		return deny("ORDER_TYPE", "only bracket-protected entries are allowed, got %q", in.OrderType)
	case !r.p.AllowedTokens[in.Token]:
		return deny("TOKEN", "token %s is not on the allowlist", in.Token)
	case in.RiskFlagged:
		return deny("RISK_FLAGGED", "Flash flags %s as risky; it may be impossible to sell", in.Token)
	case r.p.MinLiquidityUSD > 0 && in.Liquidity < r.p.MinLiquidityUSD:
		return deny("LIQUIDITY", "liquidity $%.0f is below the $%.0f minimum, exit not guaranteed", in.Liquidity, r.p.MinLiquidityUSD)
	case r.p.MaxDrawdownPct > 0 && in.AgentDrawdownPct > r.p.MaxDrawdownPct:
		return deny("CIRCUIT_BREAKER", "agent is down %.1f%% from its peak (limit %.0f%%); trading and copying paused", in.AgentDrawdownPct, r.p.MaxDrawdownPct)
	case in.Price <= 0:
		return deny("PRICE", "missing reference price")
	case in.StopLoss <= 0 || in.StopLoss >= in.Price:
		return deny("STOP_LOSS", "stop loss is mandatory and must be below price")
	case in.TakeProfit <= in.Price*(1+r.p.MinTPPct/100):
		return deny("TAKE_PROFIT", "take profit must be at least %.1f%% above price", r.p.MinTPPct)
	case (in.Price-in.StopLoss)/in.Price*100 > r.p.MaxSLPct:
		return deny("RISK", "stop loss is more than %.1f%% below price", r.p.MaxSLPct)
	case walletUSDC <= 0 && in.SizeUSD <= 0:
		return deny("BALANCE", "wallet has no USDC yet; fund it on Base to trade")
	case in.SizeUSD <= 0:
		return deny("SIZE", "trade size is zero")
	case in.SizeUSD > r.p.MaxTradeUSD:
		return deny("SIZE", "trade $%.2f exceeds max $%.2f", in.SizeUSD, r.p.MaxTradeUSD)
	case !in.OwnSize && walletUSDC > 0 && in.SizeUSD/walletUSDC*100 > r.p.MaxPositionPct+0.01:
		return deny("SIZE", "trade is %.0f%% of balance, max %.0f%%", in.SizeUSD/walletUSDC*100, r.p.MaxPositionPct)
	case in.SizeUSD > walletUSDC:
		return deny("BALANCE", "insufficient USDC: have $%.2f", walletUSDC)
	case r.count[in.WalletID] >= dailyLimit(r.p.MaxTradesPerDay, in.MaxTradesPerDay):
		return deny("DAILY_LIMIT", "%d trades today already", r.count[in.WalletID])
	case r.p.MaxOpenPerAgent > 0 && r.open[in.WalletID] >= r.p.MaxOpenPerAgent:
		return deny("OPEN_POSITION", "already has %d open position(s)", r.open[in.WalletID])
	}
	return Decision{OK: true}
}

func (r *Referee) Commit(walletID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollDay()
	r.count[walletID]++
	r.open[walletID]++
}

// MarkOpen restores an open position after a restart (no daily count).
func (r *Referee) MarkOpen(walletID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.open[walletID]++
}

func (r *Referee) Closed(walletID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open[walletID] > 0 {
		r.open[walletID]--
	}
}

// protectedTypes are the entries the arena allows: every one carries a stop
// loss, either attached on the Flash order or (stop buys) placed on the fill.
var protectedTypes = map[string]bool{
	"market+bracket": true, "limit+bracket": true, "twap+bracket": true, "stop+protect": true,
}

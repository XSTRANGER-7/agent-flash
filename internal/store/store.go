// Package store is the hot, local state of the arena (trades, follows,
// predictions, equity history). It is in-memory with atomic JSON snapshots
// to disk. Public, verifiable records are mirrored to Arkiv via the outbox;
// this store is the fast path the API reads from.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/atomicfile"
)

// Step is one entry in a trade's receipt timeline (quote -> fill -> close).
type Step struct {
	At     time.Time `json:"at"`
	Step   string    `json:"step"`
	Detail string    `json:"detail,omitempty"`
}

type Trade struct {
	ID         string  `json:"id"`
	AgentID    string  `json:"agentId"`
	WalletID   string  `json:"walletId"`
	CopyOf     string  `json:"copyOf,omitempty"`
	OrderID    string  `json:"orderId"`
	Token      string  `json:"token"`
	OrderType  string  `json:"orderType"`
	SizeUSD    float64 `json:"sizeUsd"`
	Entry      float64 `json:"entry"`
	TakeProfit float64 `json:"takeProfit"`
	StopLoss   float64 `json:"stopLoss"`
	Reason     string  `json:"reason"`
	// placed | filled | closed | cancelled | rejected | terminated | unprotected | dryrun
	Status      string `json:"status"`
	CloseReason string `json:"closeReason,omitempty"` // human text
	CloseCode   string `json:"closeCode,omitempty"`   // Flash REASON_* when there is one
	DryRun      bool   `json:"dryRun"`
	Paper       bool   `json:"paper,omitempty"` // mock money, real Flash quotes (MODE=paper)
	Test        bool   `json:"test,omitempty"`  // operator test trade (small size, no copies)
	Kind        string `json:"kind,omitempty"`  // agent | copy | invest | test | plan
	PlanID      string `json:"planId,omitempty"`
	TokenAddr   string `json:"tokenAddress,omitempty"` // empty = WETH (older trades)
	TokenDec    int    `json:"tokenDecimals,omitempty"`
	// Advanced Flash order styles: market | limit | twap (DCA) | stop (stop buy).
	Style       string   `json:"style,omitempty"`
	StopPrice   float64  `json:"stopPrice,omitempty"`   // stop buy trigger
	TwapMinutes int      `json:"twapMinutes,omitempty"` // twap duration
	ProtectMode string   `json:"protectMode,omitempty"` // bracket | trailing | separate (stop buys get it after the fill)
	TPOrderID   string   `json:"tpOrderId,omitempty"`   // separate mode: the standalone take-profit order (BracketOrderID holds the stop-loss)
	TrailPct    float64  `json:"trailPct,omitempty"`    // trailing stop distance
	USDCBefore  float64  `json:"usdcBefore"`
	PnLUSD      *float64 `json:"pnlUsd,omitempty"`      // realized, set when the position is sold
	QuotedPrice float64  `json:"quotedPrice,omitempty"` // USDC spent / WETH received (all-in) from the quote
	FeeUSD      float64  `json:"feeUsd,omitempty"`      // fees.estimatedFeeNotional

	// Flash execution receipt.
	OriginalTP     float64 `json:"originalTp,omitempty"`     // strategy target before net-of-cost adjustment
	MarkPrice      float64 `json:"markPrice,omitempty"`      // Flash valuation at quote (to.notional / to.amount)
	EntryCostUSD   float64 `json:"entryCostUsd,omitempty"`   // from.notional - to.notional
	EntryCostPct   float64 `json:"entryCostPct,omitempty"`   // of the amount spent
	BreakEvenPrice float64 `json:"breakEvenPrice,omitempty"` // price where the round trip pays for itself
	FillPrice      float64 `json:"fillPrice,omitempty"`      // entry filled.averageNotionalPrice
	ExitPrice      float64 `json:"exitPrice,omitempty"`      // exit filled.averageNotionalPrice
	WETHAmount     float64 `json:"wethAmount,omitempty"`     // bought (filled.targetAmount)
	USDCSpent      float64 `json:"usdcSpent,omitempty"`      // entry filled.contraAmount
	USDCReceived   float64 `json:"usdcReceived,omitempty"`   // exit filled.contraAmount
	BracketOrderID string  `json:"bracketOrderId,omitempty"` // protective order, once it activates
	ExitOrderID    string  `json:"exitOrderId,omitempty"`    // operator "exit now" market sell
	Protection     string  `json:"protection,omitempty"`     // pending | active | none
	EntryTx        string  `json:"entryTx,omitempty"`        // Base tx hash of the entry fill
	ExitTx         string  `json:"exitTx,omitempty"`         // Base tx hash of the exit fill
	ApproveTx      string  `json:"approveTx,omitempty"`      // capped USDC approve, when one was needed
	StopMoved      bool    `json:"stopMoved,omitempty"`      // stop loss moved to break-even (PATCH)

	Timeline  []Step    `json:"timeline,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Open is true while the trade holds (or may still buy) WETH.
func (t *Trade) Open() bool {
	return t.Status == "placed" || t.Status == "filled" || t.Status == "unprotected"
}

type Follow struct {
	FollowerID     string  `json:"followerId"`
	AgentID        string  `json:"agentId"`
	AllocationPct  float64 `json:"allocationPct"` // % of follower USDC per copied trade (scaled by agent size)
	MaxPerTradeUSD float64 `json:"maxPerTradeUsd"`
	DailyCapUSD    float64 `json:"dailyCapUsd"`
	// Mode "copy" mirrors trades the agent places from its own wallet; "invest"
	// lets the agent trade this wallet directly on every signal, even when the
	// agent wallet itself has no money.
	Mode      string    `json:"mode,omitempty"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
}

// WalletProfile is the owner-facing side of a sub-wallet: who it belongs to,
// where its money may go, its limits and whether it may trade.
type WalletProfile struct {
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"` // display name of the person
	// Payout is the only address withdrawals can go to. It is set once (when
	// the wallet is created, or with `arena payout` on the server) and the API
	// can never change it, so a stolen admin token cannot redirect funds.
	Payout       string    `json:"payout,omitempty"`
	LossLimitPct float64   `json:"lossLimitPct,omitempty"` // pause when equity falls this far below its peak
	Paused       bool      `json:"paused,omitempty"`
	PausedReason string    `json:"pausedReason,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Prediction struct {
	TradeID   string    `json:"tradeId"`
	Voter     string    `json:"voter"`
	Win       bool      `json:"win"`
	CreatedAt time.Time `json:"createdAt"`
}

type EquityPoint struct {
	At  time.Time `json:"at"`
	USD float64   `json:"usd"`
}

type snapshot struct {
	Trades      map[string]*Trade         `json:"trades"`
	Follows     map[string]*Follow        `json:"follows"` // key follower|agent
	Predictions map[string]*Prediction    `json:"predictions"`
	Equity      map[string][]EquityPoint  `json:"equity"`
	CopySpend   map[string]float64        `json:"copySpend"` // follower|day
	Profiles    map[string]*WalletProfile `json:"profiles"`
	Plans       map[string]*Plan          `json:"plans"`
	Flows       map[string][]Flow         `json:"flows"`                 // deposits (+) / withdrawals (-) per wallet
	SelfWallets map[string]string         `json:"selfWallets,omitempty"` // id -> address of people's own wallets
}

type Store struct {
	mu   sync.RWMutex
	path string
	s    snapshot
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, "state.json"), s: snapshot{
		Trades: map[string]*Trade{}, Follows: map[string]*Follow{}, Predictions: map[string]*Prediction{},
		Equity: map[string][]EquityPoint{}, CopySpend: map[string]float64{}, Profiles: map[string]*WalletProfile{}, Plans: map[string]*Plan{}, Flows: map[string][]Flow{},
	}}
	b, from, err := atomicfile.Read(st.path)
	switch {
	case err == nil:
		if jerr := json.Unmarshal(b, &st.s); jerr != nil {
			// A torn write: try the previous good snapshot before giving up.
			bak, berr := os.ReadFile(st.path + ".bak")
			if berr != nil || json.Unmarshal(bak, &st.s) != nil {
				return nil, fmt.Errorf("%s is damaged (%v) and has no usable .bak; restore it from a backup", from, jerr)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// first run
	default:
		return nil, fmt.Errorf("%s: %w; restore it from a backup", st.path, err)
	}
	return st, nil
}

// Save writes an atomic snapshot (tmp file + rename).
func (st *Store) Save() error {
	st.mu.RLock()
	b, err := json.Marshal(st.s)
	st.mu.RUnlock()
	if err != nil {
		return err
	}
	return atomicfile.Write(st.path, b, 0o600) // fsynced; the last good copy stays in state.json.bak
}

func (st *Store) PutTrade(t *Trade) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := *t
	st.s.Trades[t.ID] = &cp
}

func (st *Store) UpdateTrade(id string, fn func(*Trade)) (*Trade, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.s.Trades[id]
	if !ok {
		return nil, false
	}
	fn(t)
	t.UpdatedAt = time.Now().UTC()
	cp := *t
	return &cp, true
}

func (st *Store) Trade(id string) (*Trade, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	t, ok := st.s.Trades[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// Trades returns newest first, filtered by agent (optional).
func (st *Store) Trades(agentID string, limit int) []Trade {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []Trade
	for _, t := range st.s.Trades {
		if agentID == "" || t.AgentID == agentID {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// OpenTrades are placed but not terminal.
func (st *Store) OpenTrades() []Trade {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []Trade
	for _, t := range st.s.Trades {
		if t.Open() {
			out = append(out, *t)
		}
	}
	return out
}

func followKey(f, a string) string { return f + "|" + a }

func (st *Store) PutFollow(f *Follow) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := *f
	st.s.Follows[followKey(f.FollowerID, f.AgentID)] = &cp
}

func (st *Store) Unfollow(follower, agent string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	f, ok := st.s.Follows[followKey(follower, agent)]
	if ok {
		f.Active = false
	}
	return ok
}

func (st *Store) Followers(agentID string) []Follow {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []Follow
	for _, f := range st.s.Follows {
		if f.AgentID == agentID && f.Active {
			out = append(out, *f)
		}
	}
	return out
}

func (st *Store) FollowerCount(agentID string) int { return len(st.Followers(agentID)) }

// ReserveCopySpend atomically checks and reserves a follower's daily cap.
func (st *Store) ReserveCopySpend(follower string, usd, cap float64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := follower + "|" + time.Now().UTC().Format("2006-01-02")
	if cap > 0 && st.s.CopySpend[k]+usd > cap {
		return false
	}
	st.s.CopySpend[k] += usd
	return true
}

func (st *Store) ReleaseCopySpend(follower string, usd float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := follower + "|" + time.Now().UTC().Format("2006-01-02")
	st.s.CopySpend[k] -= usd
}

var ErrDuplicateVote = errors.New("already voted on this trade")

func (st *Store) Predict(p *Prediction) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := p.TradeID + "|" + p.Voter
	if _, dup := st.s.Predictions[k]; dup {
		return ErrDuplicateVote
	}
	if _, ok := st.s.Trades[p.TradeID]; !ok {
		return errors.New("trade not found")
	}
	cp := *p
	st.s.Predictions[k] = &cp
	return nil
}

func (st *Store) PredictionTally(tradeID string) (win, lose int) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for _, p := range st.s.Predictions {
		if p.TradeID == tradeID {
			if p.Win {
				win++
			} else {
				lose++
			}
		}
	}
	return
}

// AddEquity appends a point and keeps the last 10k per wallet.
func (st *Store) AddEquity(walletID string, usd float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pts := append(st.s.Equity[walletID], EquityPoint{At: time.Now().UTC(), USD: usd})
	if len(pts) > 10000 {
		pts = pts[len(pts)-10000:]
	}
	st.s.Equity[walletID] = pts
}

func (st *Store) Equity(walletID string) []EquityPoint {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return append([]EquityPoint(nil), st.s.Equity[walletID]...)
}

// Profile returns a copy of a wallet's profile.
func (st *Store) Profile(id string) (WalletProfile, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	p, ok := st.s.Profiles[id]
	if !ok {
		return WalletProfile{ID: id}, false
	}
	return *p, true
}

// PutProfile stores a new profile. An existing payout address is never
// replaced here (see SetPayout).
func (st *Store) PutProfile(p WalletProfile) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if old, ok := st.s.Profiles[p.ID]; ok && old.Payout != "" {
		p.Payout = old.Payout
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	st.s.Profiles[p.ID] = &p
}

// UpdateProfile changes limits / pause state. The payout address is kept.
func (st *Store) UpdateProfile(id string, fn func(*WalletProfile)) WalletProfile {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.s.Profiles[id]
	if !ok {
		p = &WalletProfile{ID: id, CreatedAt: time.Now().UTC()}
		st.s.Profiles[id] = p
	}
	payout := p.Payout
	fn(p)
	p.ID, p.Payout = id, payout
	return *p
}

// SetPayout is for the server CLI only (`arena payout`), never the API.
func (st *Store) SetPayout(id, addr string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.s.Profiles[id]
	if !ok {
		p = &WalletProfile{ID: id, CreatedAt: time.Now().UTC()}
		st.s.Profiles[id] = p
	}
	p.Payout = addr
}

// CopySpentToday is how much of a wallet's daily cap is reserved today.
func (st *Store) CopySpentToday(id string) float64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.CopySpend[id+"|"+time.Now().UTC().Format("2006-01-02")]
}

// FollowsOf lists a wallet's active follows (copy or invest).
func (st *Store) FollowsOf(walletID string) []Follow {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []Follow
	for _, f := range st.s.Follows {
		if f.FollowerID == walletID && f.Active {
			out = append(out, *f)
		}
	}
	return out
}

// Plan is a person's own automated strategy: their coin, size, targets,
// slippage, limits and how long it runs. The engine trades the plan's
// sub-wallet on its signals, through the same referee and signer.
type Plan struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Owner           string  `json:"owner"`
	WalletID        string  `json:"walletId"`
	Token           string  `json:"token"`                 // symbol, e.g. WETH or cbBTC
	Strategy        string  `json:"strategy"`              // auto | trend | dip | contrarian | interval
	IntervalMin     int     `json:"intervalMin,omitempty"` // interval strategy: minutes between entries
	TradeUSD        float64 `json:"tradeUsd"`              // per trade
	BudgetUSD       float64 `json:"budgetUsd"`             // most the plan may lose before it stops
	TakeProfitPct   float64 `json:"takeProfitPct"`
	StopLossPct     float64 `json:"stopLossPct"`
	MaxSlippagePct  float64 `json:"maxSlippagePct"`
	MaxTradesPerDay int     `json:"maxTradesPerDay"`
	LossLimitPct    float64 `json:"lossLimitPct"` // stop when realized P&L <= -X% of budget
	ExitAtEnd       bool    `json:"exitAtEnd"`
	// How each entry is placed on Flash: market | limit (LimitPct below) |
	// twap (DCA over TwapMinutes) | stop (stop buy StopPct above).
	Style        string    `json:"style,omitempty"`
	LimitPct     float64   `json:"limitPct,omitempty"`
	StopPct      float64   `json:"stopPct,omitempty"`
	TwapMinutes  int       `json:"twapMinutes,omitempty"`
	ProtectMode  string    `json:"protect,omitempty"` // bracket | trailing (stop buys)
	TrailPct     float64   `json:"trailPct,omitempty"`
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	Status       string    `json:"status"` // active | paused | ended | stopped
	StatusReason string    `json:"statusReason,omitempty"`
	LastSignal   string    `json:"lastSignal,omitempty"` // what the plan last decided, for the UI
	LastSignalAt time.Time `json:"lastSignalAt,omitempty"`
	LastTradeAt  time.Time `json:"lastTradeAt,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (st *Store) PutPlan(p Plan) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Plans == nil {
		st.s.Plans = map[string]*Plan{}
	}
	st.s.Plans[p.ID] = &p
}

func (st *Store) Plan(id string) (Plan, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	p, ok := st.s.Plans[id]
	if !ok {
		return Plan{}, false
	}
	return *p, true
}

// Plans lists every plan, newest first.
func (st *Store) Plans() []Plan {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]Plan, 0, len(st.s.Plans))
	for _, p := range st.s.Plans {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (st *Store) UpdatePlan(id string, fn func(*Plan)) (Plan, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.s.Plans[id]
	if !ok {
		return Plan{}, false
	}
	fn(p)
	p.ID = id
	return *p, true
}

// Flow is money moved into (+) or out of (-) a wallet from outside trading.
// Returns and drawdowns must not count it: a deposit is not a profit and a
// withdrawal is not a loss.
type Flow struct {
	At  time.Time `json:"at"`
	USD float64   `json:"usd"`
}

func (st *Store) AddFlow(walletID string, usd float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Flows == nil {
		st.s.Flows = map[string][]Flow{}
	}
	st.s.Flows[walletID] = append(st.s.Flows[walletID], Flow{At: time.Now().UTC(), USD: usd})
}

// FlowsSince sums a wallet's flows after t.
func (st *Store) FlowsSince(walletID string, t time.Time) float64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	sum := 0.0
	for _, f := range st.s.Flows[walletID] {
		if f.At.After(t) {
			sum += f.USD
		}
	}
	return sum
}

// LastEquity is the newest equity sample of a wallet.
func (st *Store) LastEquity(walletID string) (EquityPoint, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	pts := st.s.Equity[walletID]
	if len(pts) == 0 {
		return EquityPoint{}, false
	}
	return pts[len(pts)-1], true
}

// PerfEquity is the equity series with deposits and withdrawals taken out:
// capital (first value + later deposits) plus trading P&L only. Returns,
// drawdowns, loss limits and the circuit breaker all read this one.
func (st *Store) PerfEquity(walletID string) []EquityPoint {
	st.mu.RLock()
	defer st.mu.RUnlock()
	pts := st.s.Equity[walletID]
	if len(pts) == 0 {
		return nil
	}
	flows := st.s.Flows[walletID]
	first := pts[0]
	capital := first.USD
	for _, f := range flows {
		if f.At.After(first.At) && f.USD > 0 {
			capital += f.USD
		}
	}
	out := make([]EquityPoint, len(pts))
	for i, p := range pts {
		cum := 0.0
		for _, f := range flows {
			if f.At.After(first.At) && !f.At.After(p.At) {
				cum += f.USD
			}
		}
		out[i] = EquityPoint{At: p.At, USD: capital + (p.USD - cum - first.USD)}
	}
	return out
}

// AddSelfWallet remembers a person's own address (re-watched on restart).
func (st *Store) AddSelfWallet(id, addr string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.SelfWallets == nil {
		st.s.SelfWallets = map[string]string{}
	}
	st.s.SelfWallets[id] = addr
}

func (st *Store) SelfWallets() map[string]string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := map[string]string{}
	for k, v := range st.s.SelfWallets {
		out[k] = v
	}
	return out
}

// ResetPerformance clears equity history and flows (used when paper money
// was reset, so old balances do not read as losses or gains).
func (st *Store) ResetPerformance() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Equity = map[string][]EquityPoint{}
	st.s.Flows = map[string][]Flow{}
}

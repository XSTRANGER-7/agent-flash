package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/approval"
	"github.com/vikram/agent-arena/internal/arena"
	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/feed"
	"github.com/vikram/agent-arena/internal/flash"
	"github.com/vikram/agent-arena/internal/outbox"
	"github.com/vikram/agent-arena/internal/referee"
	"github.com/vikram/agent-arena/internal/store"
	"github.com/vikram/agent-arena/internal/wallet"
)

// Shapes below mirror real Flash responses captured on Base (Nov 2026):
// FlashOrder typed data, chainId as a string, legs with notionals,
// GET /orders/{id} -> {"order":..., "fills":[...]}.
const (
	settlement = "0x5d00000873b6BF41539e6f5365B0Ff7d3c368f78"
	vault      = "0x4cdce11ed78eeb61C52B4811d5eDe74CaC5ab893"
	usdcAddr   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	wethAddr   = "0x4200000000000000000000000000000000000006"
	cbbtcAddr  = "0xcbB7C0000aB88B473b1f5aFd9ef808440eed33Bf"
)

func flashOrderTD(funder, from, to, amount, vaultAddr, contract string, deadline int64) string {
	return `{"types":{"EIP712Domain":[{"name":"name","type":"string"},{"name":"version","type":"string"},{"name":"chainId","type":"uint256"},{"name":"verifyingContract","type":"address"}],` +
		`"FlashOrder":[{"name":"swapper","type":"address"},{"name":"vault","type":"address"},{"name":"recipient","type":"address"},{"name":"fromToken","type":"address"},{"name":"toToken","type":"address"},{"name":"fromAmount","type":"uint256"},{"name":"salt","type":"uint256"},{"name":"deadline","type":"uint256"}]},` +
		`"primaryType":"FlashOrder","domain":{"name":"DefinitiveFlashAllowance","version":"1","chainId":"8453","verifyingContract":"` + contract + `"},` +
		`"message":{"deadline":"` + strconv.FormatInt(deadline, 10) + `","fromAmount":"` + amount + `","fromToken":"` + from + `","recipient":"` + funder + `","salt":"2168727478726192177623442429959449342720","swapper":"` + funder + `","toToken":"` + to + `","vault":"` + vaultAddr + `"}}`
}

// units converts a decimal string to base units exactly.
func units(s string, dec int) string {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad decimal " + s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)))
	return new(big.Int).Quo(r.Num(), r.Denom()).String()
}

type fakeOrder struct {
	ID, Funder, Kind, Status, Reason string
	BracketID, BracketStatus         string
	Target, Contra, Avg              float64
	Tx                               string
}

// fakeFlash mimics the Flash endpoints the engine uses and verifies every
// signature it receives (EIP-712 orders, EIP-191 cancel / update).
type fakeFlash struct {
	t       *testing.T
	keys    map[string]*evm.PrivateKey // address(lower) -> key
	quotes  atomic.Int32
	orders  atomic.Int32
	badSigs atomic.Int32

	mu         sync.Mutex
	usdc, weth map[string]float64
	quotePx    float64 // Flash mark (0 = 3000)
	searchPx   float64 // /search price, the test reference (0 = 3000)
	costFrac   float64 // all-in cost as a fraction of the amount spent
	risky      bool
	setupTxs   []string
	vaultAddr  string
	bracketTD  map[string]string // quoteId -> bracket typed data
	state      map[string]*fakeOrder
	lastOrder  map[string]any
	cancels    []string
	patches    []map[string]any
	lastSide   string
}

func (f *fakeFlash) px() float64 {
	if f.quotePx == 0 {
		return 3000
	}
	return f.quotePx
}

func (f *fakeFlash) verify(digest []byte, funder, sigHex string) {
	key := f.keys[strings.ToLower(funder)]
	sig, _ := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if key == nil || !key.Verify(digest, sig) {
		f.badSigs.Add(1)
	}
}

func (f *fakeFlash) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-definitive-api-key") != "dpka_test" {
		w.WriteHeader(401)
		return
	}
	w.Header().Set("RateLimit-Policy", "50;w=1")
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/search":
		sp := f.searchPx
		if sp == 0 {
			sp = 3000
		}
		addr := r.URL.Query().Get("query")
		if !strings.HasPrefix(addr, "0x") {
			addr = wethAddr
		}
		io.WriteString(w, `{"assets":[{"chain":"base","address":"`+addr+`","symbol":"WETH","decimals":18,"price":"`+strconv.FormatFloat(sp, 'f', 2, 64)+`","liquidity":"47702954","riskFlagged":`+strconv.FormatBool(f.risky)+`}]}`)
	case strings.HasPrefix(path, "/balances/"):
		addr := strings.ToLower(strings.TrimPrefix(path, "/balances/"))
		fmt.Fprintf(w, `{"balances":[{"chain":"base","address":"%s","symbol":"USDC","balance":"%f"},{"chain":"base","address":"%s","symbol":"WETH","balance":"%.18f"}]}`, usdcAddr, f.usdc[addr], wethAddr, f.weth[addr])
	case path == "/quote":
		f.quote(w, r)
	case path == "/order":
		f.order(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/orders/"):
		o := f.state[strings.TrimPrefix(path, "/orders/")]
		if o == nil || !strings.EqualFold(o.Funder, r.URL.Query().Get("funderAddress")) {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"order not found"}}`)
			return
		}
		f.writeOrder(w, o)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/orders/"), "/cancel")
		var body struct{ CancelMessage, UserSignature string }
		json.NewDecoder(r.Body).Decode(&body)
		o := f.state[id]
		if o == nil || body.CancelMessage != flash.CancelMessage(id) {
			w.WriteHeader(400)
			return
		}
		f.verify(evm.PersonalMessageHash([]byte(body.CancelMessage)), o.Funder, body.UserSignature)
		if o.Status == flash.StatusFilled {
			w.WriteHeader(422)
			io.WriteString(w, `{"error":{"code":"FAILED_PRECONDITION","message":"order already filled"}}`)
			return
		}
		o.Status, o.Reason = flash.StatusCancelled, "REASON_USER_REQUESTED"
		f.cancels = append(f.cancels, id)
		io.WriteString(w, `{"ok":true}`)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/orders/"):
		id := strings.TrimPrefix(path, "/orders/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		o := f.state[id]
		msg, _ := body["updateMessage"].(string)
		sig, _ := body["userSignature"].(string)
		if o == nil || !strings.HasPrefix(msg, "Definitive Flash — Update Order\nOrder: "+id+"\nIssued At: ") {
			w.WriteHeader(400)
			return
		}
		f.verify(evm.PersonalMessageHash([]byte(msg)), o.Funder, sig)
		f.patches = append(f.patches, body)
		io.WriteString(w, `{"ok":true}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeFlash) quote(w http.ResponseWriter, r *http.Request) {
	var q flash.QuoteRequest
	json.NewDecoder(r.Body).Decode(&q)
	if q.FlashIntegratorFeeBps == "" || !q.ForceMinimalAllowance || q.EVMUsePermit2 == nil || *q.EVMUsePermit2 || (q.MaxSlippage == "" && q.OrderType == "market") {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"VALIDATION_ERROR","message":"bad quote"}}`)
		return
	}
	f.quotes.Add(1)
	f.lastSide = q.Side
	px, cost := f.px(), f.costFrac
	qty, _ := strconv.ParseFloat(q.Qty, 64)
	deadline := time.Now().Add(5 * time.Minute).Unix()
	tgt, dec := q.TargetAsset, 18
	if strings.EqualFold(tgt, cbbtcAddr) {
		dec = 8
	}
	fmtAmt := func(x float64) string {
		m := math.Pow10(dec)
		return strconv.FormatFloat(math.Floor(x*m)/m, 'f', dec, 64)
	}
	id := fmt.Sprintf("q_%d", f.quotes.Load())
	resp := map[string]any{"quoteId": id, "bridgeQuoteId": nil, "orderType": q.OrderType, "side": q.Side, "wrap": nil, "svm": nil,
		"estimatedPriceImpact": "0.0001", "recommendedSlippage": "0.15", "fees": map[string]any{"estimatedFeeNotional": strconv.FormatFloat(qty*0.003, 'f', 4, 64)}}
	if f.setupTxs != nil {
		resp["setupTxs"] = f.setupTxs
	} else {
		resp["setupTxs"] = nil
	}
	if q.Side == "sell" {
		resp["from"] = map[string]any{"asset": "target", "amount": q.Qty, "notional": strconv.FormatFloat(qty*px, 'f', 6, 64)}
		resp["to"] = map[string]any{"asset": "contra", "amount": strconv.FormatFloat(qty*px*(1-cost), 'f', 6, 64), "notional": strconv.FormatFloat(qty*px*(1-cost), 'f', 6, 64)}
		resp["evm"] = map[string]any{"approveTx": nil, "permitTypedData": "", "orderTypedData": flashOrderTD(q.FunderAddress, tgt, usdcAddr, units(q.Qty, dec), f.vaultAddr, settlement, deadline)}
	} else {
		got := qty * (1 - cost) / px
		resp["from"] = map[string]any{"asset": "contra", "amount": q.Qty, "notional": q.Qty}
		resp["to"] = map[string]any{"asset": "target", "amount": fmtAmt(got), "notional": strconv.FormatFloat(qty*(1-cost), 'f', 6, 64)}
		resp["evm"] = map[string]any{"approveTx": nil, "permitTypedData": "", "orderTypedData": flashOrderTD(q.FunderAddress, usdcAddr, tgt, units(q.Qty, 6), f.vaultAddr, settlement, deadline)}
		if q.AttachedBracket != nil {
			max := fmtAmt(got * 1.07)
			btd := flashOrderTD(q.FunderAddress, tgt, usdcAddr, units(max, dec), f.vaultAddr, settlement, time.Now().Add(72*time.Hour).Unix())
			f.bracketTD[id] = btd
			resp["attachedBracket"] = map[string]any{"evm": map[string]any{"approveTx": nil, "permitTypedData": nil, "orderTypedData": btd},
				"svm": nil, "salt": "14914188296754051416344544069108673287693612", "deadline": "1794188524", "signedMaxFromAmount": max}
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeFlash) order(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	raw := map[string]any{}
	json.Unmarshal(body, &raw)
	f.lastOrder = raw
	for _, k := range []string{"expireTime", "evmUsePermit2", "forceMinimalAllowance", "durationSeconds"} {
		if _, ok := raw[k]; ok {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"error":{"code":"VALIDATION_ERROR","message":"unknown field %s"}}`, k)
			return
		}
	}
	var o flash.OrderRequest
	json.Unmarshal(body, &o)
	td, _ := evm.ParseTypedData(o.EVMOrderTypedData)
	d, _ := td.Digest()
	f.verify(d, o.FunderAddress, o.UserSignature)
	if btd, ok := f.bracketTD[o.QuoteID]; ok {
		if o.AttachedBracket == nil || o.AttachedBracket.Salt == "" || o.AttachedBracket.Deadline == "" || o.AttachedBracket.SignedMaxFromAmount == "" {
			w.WriteHeader(400)
			return
		}
		btdP, _ := evm.ParseTypedData(btd)
		bd, _ := btdP.Digest()
		f.verify(bd, o.FunderAddress, o.AttachedBracket.UserSignature)
	}
	if o.FlashIntegratorFeeBps != "30" {
		w.WriteHeader(400)
		return
	}
	n := f.orders.Add(1)
	id := fmt.Sprintf("ord_%d", n)
	kind := "entry"
	if o.Side == "sell" {
		kind = "exit"
	}
	f.state[id] = &fakeOrder{ID: id, Funder: o.FunderAddress, Kind: kind, Status: flash.StatusAccepted, BracketStatus: "pending_activation"}
	w.WriteHeader(201)
	if o.AttachedBracket != nil {
		fmt.Fprintf(w, `{"orderId":"%s","attachedBracket":{"status":"pending_activation"}}`, id)
	} else {
		fmt.Fprintf(w, `{"orderId":"%s"}`, id)
	}
}

func (f *fakeFlash) writeOrder(w http.ResponseWriter, o *fakeOrder) {
	ord := map[string]any{"orderId": o.ID, "status": o.Status, "closeReason": nil, "orderType": "market", "side": "buy", "qty": "4", "funderAddress": o.Funder, "filled": nil}
	if o.Reason != "" {
		ord["closeReason"] = o.Reason
	}
	if o.Kind == "bracket" || o.Kind == "exit" {
		ord["side"] = "sell"
	}
	if o.Kind == "bracket" {
		ord["orderType"] = "bracket"
	}
	var fills []any
	if o.Target > 0 {
		ord["filled"] = map[string]any{"targetAmount": strconv.FormatFloat(o.Target, 'f', 18, 64), "contraAmount": strconv.FormatFloat(o.Contra, 'f', 6, 64), "averagePrice": nil, "averageNotionalPrice": strconv.FormatFloat(o.Avg, 'f', 2, 64)}
		fills = append(fills, map[string]any{"status": "FILL_STATUS_SETTLED", "orderId": o.ID, "transactionId": o.Tx, "fillPrice": strconv.FormatFloat(o.Avg, 'f', 2, 64), "feeNotional": "0.01", "integratorFeeNotional": "0.012", "contraAmount": strconv.FormatFloat(o.Contra, 'f', 6, 64), "targetAmount": strconv.FormatFloat(o.Target, 'f', 18, 64)})
	}
	if o.Kind == "entry" {
		ab := map[string]any{"status": o.BracketStatus, "bracketOrderId": nil, "takeProfit": map[string]any{"notionalPrice": "3114.00"}, "stopLoss": map[string]any{"notionalPrice": "2940.00"}}
		if o.BracketID != "" {
			ab["bracketOrderId"] = o.BracketID
		}
		ord["attachedBracket"] = ab
	}
	json.NewEncoder(w).Encode(map[string]any{"order": ord, "fills": fills})
}

// fill marks an entry filled: the wallet swaps USDC for WETH and the bracket activates.
func (f *fakeFlash) fill(id string, avg float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.state[id]
	spent := 4.0
	o.Status, o.Target, o.Contra, o.Avg, o.Tx = flash.StatusFilled, spent*(1-f.costFrac)/avg, spent, avg, "0xentrytx"
	o.BracketID, o.BracketStatus = "brk_"+id, "active"
	a := strings.ToLower(o.Funder)
	f.usdc[a] -= spent
	f.weth[a] += o.Target
	f.state[o.BracketID] = &fakeOrder{ID: o.BracketID, Funder: o.Funder, Kind: "bracket", Status: flash.StatusAccepted}
}

// exit sells the position of order id at px (TP/SL leg or operator market sell).
func (f *fakeFlash) exit(id string, px float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.state[id]
	a := strings.ToLower(o.Funder)
	sold := f.weth[a]
	o.Status, o.Target, o.Contra, o.Avg, o.Tx = flash.StatusFilled, sold, sold*px*(1-f.costFrac), px, "0xexittx"
	f.weth[a] = 0
	f.usdc[a] += o.Contra
}

func (f *fakeFlash) set(id string, fn func(*fakeOrder)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.state[id])
}

func setup(t *testing.T) (*Engine, *fakeFlash) {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, "wallets.json")
	if _, err := wallet.Generate(wf, []string{"trendhunter"}, []string{"follower1"}); err != nil {
		t.Fatal(err)
	}
	ws, err := wallet.Load(wf)
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeFlash{t: t, keys: map[string]*evm.PrivateKey{}, usdc: map[string]float64{}, weth: map[string]float64{},
		costFrac: 0.004, vaultAddr: vault, bracketTD: map[string]string{}, state: map[string]*fakeOrder{}}
	for _, w := range ws.List("") {
		ff.keys[strings.ToLower(w.Address())] = w.Signer.Key()
		ff.usdc[strings.ToLower(w.Address())] = 20
	}
	srv := httptest.NewServer(ff)
	t.Cleanup(srv.Close)

	cfg := &config.Config{FlashAPIKey: "dpka_test", FlashBaseURL: srv.URL, FeeBps: "30", Chain: "base", ChainID: 8453,
		USDC: usdcAddr, WETH: wethAddr, TradeAsset: "WETH",
		DryRun: false, MaxApprovalUSD: 10, ApprovalThresholdUSD: 3, ApprovalTimeout: 300 * time.Millisecond,
		MaxTradeUSD: 5, MaxPositionPct: 25, MaxTradesPerDay: 5, CopyWorkers: 2, SeasonID: "test",
		MaxPriceDeviationPct: 1.5, MinLiquidityUSD: 1_000_000, MaxAgentDrawdownPct: 20,
		MaxSlippage: "0.03", MaxEntryCostPct: 2.5, NetTargets: true, BreakEvenStop: true,
		SettlementAllowlist: []string{settlement}, PriceEvery: time.Minute, OrderPoll: time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := flash.New(srv.URL, "dpka_test", log)
	st, _ := store.Open(dir)
	ob, _ := outbox.Open(filepath.Join(dir, "outbox"), "", "", log)
	t.Cleanup(ob.Close)
	fd := feed.New(100)
	e := &Engine{Cfg: cfg, Log: log, Flash: fc, Exec: NewExecutor(cfg, fc, evm.NewRPC("http://127.0.0.1:1"), log),
		Referee:   referee.New(referee.Policy{AllowedTokens: map[string]bool{"WETH": true}, MaxTradeUSD: 5, MaxPositionPct: 25, MaxTradesPerDay: 5, MaxOpenPerAgent: 1, MinTPPct: 0.5, MaxSLPct: 5, MinLiquidityUSD: 1_000_000, MaxDrawdownPct: 20}),
		Approvals: approval.NewManager(cfg.ApprovalTimeout, nil, nil), Store: st, Feed: fd, Outbox: ob, Wallets: ws,
		Prices: arena.NewPriceFeed(fc, nil, "WETH", cfg.WETH, "base", log), Agents: arena.DefaultRoster()}
	return e, ff
}

func intent() referee.Intent {
	return referee.Intent{AgentID: "trendhunter", WalletID: "trendhunter", OrderType: "market+bracket", Token: "WETH", SizePct: 20, Price: 3000, TakeProfit: 3090, StopLoss: 2940, Reason: "test"}
}

func TestProposePlacesSignedBracketOrder(t *testing.T) {
	e, ff := setup(t)
	res := e.Propose(context.Background(), intent())
	if res.Error != "" || !res.Decision.OK || res.Trade == nil {
		t.Fatalf("propose failed: %+v", res)
	}
	if res.Trade.SizeUSD != 4 { // 20% of $20
		t.Fatalf("size: %v", res.Trade.SizeUSD)
	}
	if ff.orders.Load() != 1 || ff.badSigs.Load() != 0 {
		t.Fatalf("orders=%d badSigs=%d", ff.orders.Load(), ff.badSigs.Load())
	}
	// The submit body carries only documented SubmitOrderRequest fields.
	if ff.lastOrder["maxSlippage"] != "0.03" || ff.lastOrder["quoteId"] == nil || ff.lastOrder["evmOrderTypedData"] == nil {
		t.Fatalf("order body: %v", ff.lastOrder)
	}
	ab, _ := ff.lastOrder["attachedBracket"].(map[string]any)
	if _, has := ab["expireTime"]; has || ab["salt"] == nil || ab["userSignature"] == nil {
		t.Fatalf("bracket body: %v", ab)
	}
	// Second trade is blocked: one open position per agent.
	res2 := e.Propose(context.Background(), intent())
	if res2.Decision.OK || res2.Decision.Code != "OPEN_POSITION" {
		t.Fatalf("expected OPEN_POSITION block, got %+v", res2.Decision)
	}
}

// MIN_TRADE_USD lifts a small % slice to a size whose fixed Flash cost is
// bearable, but never above what the wallet holds.
func TestMinTradeUSDLiftsSmallSlices(t *testing.T) {
	e, ff := setup(t)
	e.Cfg.MinTradeUSD, e.Cfg.MaxPositionPct = 5, 50
	res := e.Propose(context.Background(), intent()) // 20% of $20 = $4
	if !res.Decision.OK || res.Trade == nil || res.Trade.SizeUSD != 5 {
		t.Fatalf("want a $5 trade, got %+v / %+v", res.Decision, res.Trade)
	}
	// A wallet holding less than the minimum keeps its % slice.
	w, _ := e.Wallets.Get("follower1")
	ff.mu.Lock()
	ff.usdc[strings.ToLower(w.Address())] = 3
	ff.mu.Unlock()
	in := intent()
	in.AgentID, in.WalletID = "trendhunter", "follower1"
	res = e.Propose(context.Background(), in)
	if res.Trade == nil || res.Trade.SizeUSD != 0.6 {
		t.Fatalf("want the 20%% slice ($0.60), got %+v / %+v", res.Decision, res.Trade)
	}
}

func TestNetTargetsRaiseTakeProfitByRoundTripCost(t *testing.T) {
	e, ff := setup(t)
	res := e.Propose(context.Background(), intent())
	if res.Trade == nil {
		t.Fatalf("propose failed: %+v", res)
	}
	// cost 0.4% -> round trip 0.8% of the ~$3000 mark = +$24.
	if ff.quotes.Load() != 2 || math.Abs(res.Trade.TakeProfit-3114) > 0.05 || res.Trade.OriginalTP != 3090 {
		t.Fatalf("quotes=%d tp=%v original=%v", ff.quotes.Load(), res.Trade.TakeProfit, res.Trade.OriginalTP)
	}
	if res.Trade.EntryCostPct < 0.39 || res.Trade.EntryCostPct > 0.41 || res.Trade.MarkPrice < 2999 || res.Trade.MarkPrice > 3001 {
		t.Fatalf("receipt: cost=%v mark=%v", res.Trade.EntryCostPct, res.Trade.MarkPrice)
	}
	var steps []string
	for _, s := range res.Trade.Timeline {
		steps = append(steps, s.Step)
	}
	if strings.Join(steps, ",") != "quoted,targets,signed,submitted" {
		t.Fatalf("timeline: %v", steps)
	}
}

func TestCostGuardRefusesTinyTrades(t *testing.T) {
	e, ff := setup(t)
	ff.costFrac = 0.035 // what a $2 trade really costs on Flash
	res := e.Propose(context.Background(), intent())
	if res.Trade != nil || !strings.Contains(res.Error, "cost guard") || ff.orders.Load() != 0 {
		t.Fatalf("expected cost guard refusal, got %+v orders=%d", res, ff.orders.Load())
	}
}

func TestPriceGuardRefusesDriftingQuote(t *testing.T) {
	e, ff := setup(t)
	ff.quotePx = 3100 // Flash values WETH 3.3% above the 3000 reference
	res := e.Propose(context.Background(), intent())
	if res.Trade != nil || !strings.Contains(res.Error, "price guard") || ff.orders.Load() != 0 {
		t.Fatalf("expected price guard refusal, got %+v orders=%d", res, ff.orders.Load())
	}
}

func TestRefereeBlocksNakedOrders(t *testing.T) {
	e, ff := setup(t)
	in := intent()
	in.StopLoss = 0
	if res := e.Propose(context.Background(), in); res.Decision.OK || res.Decision.Code != "STOP_LOSS" {
		t.Fatalf("expected STOP_LOSS block: %+v", res.Decision)
	}
	in = intent()
	in.OrderType = "market"
	if res := e.Propose(context.Background(), in); res.Decision.OK || res.Decision.Code != "ORDER_TYPE" {
		t.Fatalf("expected ORDER_TYPE block: %+v", res.Decision)
	}
	if ff.quotes.Load() != 0 {
		t.Fatal("blocked intents must never reach Flash")
	}
}

func TestRepeatedBlocksPostOnce(t *testing.T) {
	e, ff := setup(t)
	for a := range ff.usdc {
		ff.usdc[a] = 0 // unfunded wallet, like a fresh keygen
	}
	blocks := func() (n int) {
		for _, p := range e.Feed.Latest(100) {
			if p.Kind == "trade_blocked" {
				n++
			}
		}
		return
	}
	for i := 0; i < 5; i++ {
		if res := e.Propose(context.Background(), intent()); res.Decision.Code != "BALANCE" {
			t.Fatalf("expected BALANCE block, got %+v", res)
		}
	}
	if n := blocks(); n != 1 {
		t.Fatalf("same block posted %d times, want 1", n)
	}
	// A different reason is news and is posted.
	in := intent()
	in.StopLoss = 0
	e.Propose(context.Background(), in)
	if n := blocks(); n != 2 {
		t.Fatalf("new block code not posted: %d", n)
	}
	// After the quiet window the same block is posted again.
	e.mu.Lock()
	for k := range e.lastBlock {
		e.lastBlock[k] = time.Now().Add(-blockQuiet - time.Second)
	}
	e.mu.Unlock()
	e.Propose(context.Background(), intent())
	if n := blocks(); n != 3 {
		t.Fatalf("block not re-posted after quiet window: %d", n)
	}
	// Funding the wallet lets the agent trade, and a trade resets the memory.
	for a := range ff.usdc {
		ff.usdc[a] = 20
	}
	e.invalidate("trendhunter")
	if res := e.Propose(context.Background(), intent()); res.Trade == nil {
		t.Fatalf("funded wallet should trade: %+v", res)
	}
	e.mu.Lock()
	left := len(e.lastBlock)
	e.mu.Unlock()
	if left != 0 {
		t.Fatalf("trade should clear the wallet's block memory, %d left", left)
	}
}

func TestGuardRefusesAnythingButOurOrder(t *testing.T) {
	e, _ := setup(t)
	w, _ := e.Wallets.Get("trendhunter")
	me := w.Address()
	dl := time.Now().Add(time.Hour).Unix()
	x := expect{from: usdcAddr, to: wethAddr, exact: big.NewInt(4_000_000)}
	good := flashOrderTD(me, usdcAddr, wethAddr, "4000000", vault, settlement, dl)
	if err := e.Exec.guardTypedData(good, me, x); err != nil {
		t.Fatalf("good payload refused: %v", err)
	}
	other := "0x2222222222222222222222222222222222222222"
	cases := map[string]string{
		"foreign swapper":     flashOrderTD(other, usdcAddr, wethAddr, "4000000", vault, settlement, dl),
		"foreign recipient":   strings.Replace(good, `"recipient":"`+me, `"recipient":"`+other, 1),
		"wrong chain":         strings.Replace(good, `"chainId":"8453"`, `"chainId":"1"`, 1),
		"no chain id":         strings.Replace(good, `"chainId":"8453",`, ``, 1),
		"wrong token":         flashOrderTD(me, wethAddr, usdcAddr, "4000000", vault, settlement, dl),
		"wrong amount":        flashOrderTD(me, usdcAddr, wethAddr, "4000002", vault, settlement, dl),
		"unknown contract":    flashOrderTD(me, usdcAddr, wethAddr, "4000000", vault, other, dl),
		"vault changed":       flashOrderTD(me, usdcAddr, wethAddr, "4000000", other, settlement, dl),
		"expired":             flashOrderTD(me, usdcAddr, wethAddr, "4000000", vault, settlement, time.Now().Add(-time.Minute).Unix()),
		"unknown primaryType": strings.Replace(good, `"primaryType":"FlashOrder"`, `"primaryType":"Mail"`, 1),
		"retyped field":       strings.Replace(good, `{"name":"vault","type":"address"}`, `{"name":"vault","type":"string"}`, 1),
	}
	for name, td := range cases {
		if err := e.Exec.guardTypedData(td, me, x); err == nil {
			t.Errorf("%s: must refuse", name)
		}
	}
}

func TestSetupTxsMustBePlainApprovals(t *testing.T) {
	e, _ := setup(t)
	// Real setupTx captured from Flash: unsigned EIP-1559 approve(USDC -> settlement, 2 USDC).
	real := "0x02f87082210546830f46c283a7d8c08301078394833589fcd6edb6e08f4c7c32d4f71b54bda0291380b844095ea7b30000000000000000000000005d00000873b6bf41539e6f5365b0ff7d3c368f7800000000000000000000000000000000000000000000000000000000001e8480c0808080"
	if err := e.Exec.checkSetupTxs([]string{real}, "0x0"); err != nil {
		t.Fatalf("real approve setup tx refused: %v", err)
	}
	// Same tx but calling transfer() instead of approve().
	bad := strings.Replace(real, "b844095ea7b3", "b844a9059cbb", 1)
	if err := e.Exec.checkSetupTxs([]string{bad}, "0x0"); err == nil {
		t.Fatal("a non-approve setup tx must be refused")
	}
}

func TestLifecycleFillBracketTakeProfit(t *testing.T) {
	e, ff := setup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	id := res.Trade.ID
	e.RefreshTrade(ctx, id)
	if tr, _ := e.Store.Trade(id); tr.Status != "placed" {
		t.Fatalf("still working entry should stay placed, got %s", tr.Status)
	}
	ff.fill(res.Trade.OrderID, 3000)
	e.RefreshTrade(ctx, id)
	tr, _ := e.Store.Trade(id)
	if tr.Status != "filled" || tr.BracketOrderID != "brk_"+res.Trade.OrderID || tr.Protection != "active" || tr.EntryTx != "0xentrytx" || tr.FillPrice != 3000 || tr.USDCSpent != 4 {
		t.Fatalf("after fill: %+v", tr)
	}
	if tr.BreakEvenPrice < 3020 || tr.BreakEvenPrice > 3030 { // 3000/(1-0.004) * 1.004
		t.Fatalf("break-even %v", tr.BreakEvenPrice)
	}
	ff.exit(tr.BracketOrderID, 3120)
	e.RefreshTrade(ctx, id)
	tr, _ = e.Store.Trade(id)
	if tr.Status != "closed" || tr.CloseReason != "take profit hit" || tr.PnLUSD == nil || *tr.PnLUSD <= 0 || tr.ExitTx != "0xexittx" || tr.ExitPrice != 3120 {
		t.Fatalf("after exit: %+v pnl=%v", tr, tr.PnLUSD)
	}
	// The wallet is free again.
	if res := e.Propose(ctx, intent()); res.Trade == nil {
		t.Fatalf("wallet should be free after close: %+v", res)
	}
	if ff.badSigs.Load() != 0 {
		t.Fatalf("bad signatures: %d", ff.badSigs.Load())
	}
}

func TestBracketCancelledLeavesPositionUnprotected(t *testing.T) {
	e, ff := setup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	ff.fill(res.Trade.OrderID, 3000)
	e.RefreshTrade(ctx, res.Trade.ID)
	ff.set("brk_"+res.Trade.OrderID, func(o *fakeOrder) {
		o.Status, o.Reason = flash.StatusCancelled, "REASON_INSUFFICIENT_ASSET_BALANCE"
	})
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if tr.Status != "unprotected" || tr.Protection != "none" || tr.CloseCode != "REASON_INSUFFICIENT_ASSET_BALANCE" {
		t.Fatalf("expected unprotected: %+v", tr)
	}
	// Still counts as open: no second position on this wallet.
	if r := e.Propose(ctx, intent()); r.Decision.Code != "OPEN_POSITION" {
		t.Fatalf("unprotected position must still block: %+v", r.Decision)
	}
}

func TestExitNowCancelsBracketAndSells(t *testing.T) {
	e, ff := setup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	ff.fill(res.Trade.OrderID, 3000)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, err := e.ExitPosition(ctx, res.Trade.ID)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	if len(ff.cancels) != 1 || ff.cancels[0] != "brk_"+res.Trade.OrderID || ff.lastSide != "sell" || tr.ExitOrderID == "" {
		t.Fatalf("cancels=%v side=%s exitOrder=%s", ff.cancels, ff.lastSide, tr.ExitOrderID)
	}
	ff.exit(tr.ExitOrderID, 3010)
	e.RefreshTrade(ctx, res.Trade.ID)
	got, _ := e.Store.Trade(res.Trade.ID)
	if got.Status != "closed" || got.CloseReason != "exited by operator" || got.PnLUSD == nil {
		t.Fatalf("after exit fill: %+v", got)
	}
	if ff.badSigs.Load() != 0 {
		t.Fatalf("bad signatures: %d", ff.badSigs.Load())
	}
}

func TestStopMovesToBreakEven(t *testing.T) {
	e, ff := setup(t)
	ctx := context.Background()
	res := e.Propose(ctx, intent())
	ff.fill(res.Trade.OrderID, 3000)
	e.RefreshTrade(ctx, res.Trade.ID)
	// Price runs past halfway between break-even (~3024) and TP (~3114).
	ff.mu.Lock()
	ff.searchPx = 3080
	ff.mu.Unlock()
	e.Prices.Refresh(ctx)
	e.RefreshTrade(ctx, res.Trade.ID)
	tr, _ := e.Store.Trade(res.Trade.ID)
	if !tr.StopMoved || len(ff.patches) != 1 || math.Abs(tr.StopLoss-tr.BreakEvenPrice) > 0.01 {
		t.Fatalf("stop not moved: moved=%v patches=%d sl=%v be=%v", tr.StopMoved, len(ff.patches), tr.StopLoss, tr.BreakEvenPrice)
	}
	msg := ff.patches[0]["updateMessage"].(string)
	want := "\nTrigger Lower Notional Price: " + fmtPrice(tr.StopLoss) + "\nTrigger Upper Notional Price: " + fmtPrice(tr.TakeProfit)
	if !strings.HasSuffix(msg, want) || ff.badSigs.Load() != 0 {
		t.Fatalf("update message %q (bad sigs %d)", msg, ff.badSigs.Load())
	}
}

func TestRestoreOpenKeepsOnePositionRule(t *testing.T) {
	e, _ := setup(t)
	e.Store.PutTrade(&store.Trade{ID: "trd_old", AgentID: "trendhunter", WalletID: "trendhunter", Status: "filled", CreatedAt: time.Now()})
	e.RestoreOpen()
	if r := e.Propose(context.Background(), intent()); r.Decision.Code != "OPEN_POSITION" {
		t.Fatalf("restored position must block a second one: %+v", r.Decision)
	}
}

func TestCopyNeedsApprovalAboveThreshold(t *testing.T) {
	e, ff := setup(t)
	e.Store.PutFollow(&store.Follow{FollowerID: "follower1", AgentID: "trendhunter", AllocationPct: 20, MaxPerTradeUSD: 5, DailyCapUSD: 20, Active: true})
	e.Propose(context.Background(), intent()) // follower copy = $4 > $3 threshold -> approval, nobody approves -> expires
	e.Wait()
	if ff.orders.Load() != 1 {
		t.Fatalf("copy must not be placed without approval; orders=%d", ff.orders.Load())
	}
	reqs := e.Approvals.List(false)
	if len(reqs) != 1 || reqs[0].Status != approval.Expired {
		t.Fatalf("expected one expired approval, got %+v", reqs)
	}
}

func TestCopyApprovedIsPlaced(t *testing.T) {
	e, ff := setup(t)
	e.Store.PutFollow(&store.Follow{FollowerID: "follower1", AgentID: "trendhunter", AllocationPct: 20, MaxPerTradeUSD: 5, DailyCapUSD: 20, Active: true})
	go func() { // approve as soon as the request appears
		for i := 0; i < 100; i++ {
			for _, r := range e.Approvals.List(true) {
				e.Approvals.AdminDecide(r.ID, true)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	e.Propose(context.Background(), intent())
	e.Wait()
	if ff.orders.Load() != 2 || ff.badSigs.Load() != 0 {
		t.Fatalf("expected agent + copy orders with valid sigs; orders=%d bad=%d", ff.orders.Load(), ff.badSigs.Load())
	}
}

func TestRiskFlaggedTokenBlocked(t *testing.T) {
	e, ff := setup(t)
	ff.risky = true
	res := e.Propose(context.Background(), intent())
	if res.Decision.OK || res.Decision.Code != "RISK_FLAGGED" || ff.orders.Load() != 0 {
		t.Fatalf("expected RISK_FLAGGED, got %+v", res.Decision)
	}
}

func TestCircuitBreakerPausesAgentAndCopiers(t *testing.T) {
	e, ff := setup(t)
	for _, v := range []float64{100, 110, 80} { // 27% below peak
		e.Store.AddEquity("trendhunter", v)
	}
	res := e.Propose(context.Background(), intent())
	if res.Decision.OK || res.Decision.Code != "CIRCUIT_BREAKER" {
		t.Fatalf("expected CIRCUIT_BREAKER, got %+v", res.Decision)
	}
	copyIntent := intent()
	copyIntent.WalletID, copyIntent.CopyOf, copyIntent.SizeUSD = "follower1", "trd_x", 2
	if res := e.Propose(context.Background(), copyIntent); res.Decision.Code != "CIRCUIT_BREAKER" {
		t.Fatalf("copiers must be paused too, got %+v", res.Decision)
	}
	if ff.orders.Load() != 0 {
		t.Fatal("no orders while the breaker is tripped")
	}
}

func TestDecToUnitsIsExact(t *testing.T) {
	for _, c := range []struct {
		in   string
		dec  int
		want string
	}{{"2", 6, "2000000"}, {"2.0000", 6, "2000000"}, {"0.5", 6, "500000"}, {"4.123456", 6, "4123456"}, {"0.0009405475284212", 18, "940547528421200"}} {
		got, err := decToUnits(c.in, c.dec)
		if err != nil || got.String() != c.want {
			t.Errorf("%s -> %v (%v), want %s", c.in, got, err, c.want)
		}
	}
	if _, err := decToUnits("1.0000001", 6); err == nil {
		t.Error("extra precision must be refused")
	}
}

// Package paper is paper trading against the real Flash API: mock money,
// real everything else.
//
// The Venue sits behind the Flash client as its http.RoundTripper:
//
//	POST /quote, GET /search   forwarded to the real Flash API (real prices,
//	                           real costs, real EIP-712 payloads)
//	POST /order                the signature is verified (ecrecover over the
//	                           exact payload Flash returned) before anything
//	                           happens; market orders fill at the real quote's
//	                           net amount; limit entries rest until the onchain
//	                           price crosses them
//	attached bracket           activates on the fill; fires when the onchain
//	                           pool price crosses TP or SL; the USDC it gets
//	                           back comes from a real Flash sell quote
//	GET /orders[/{id}], cancel, batch cancel, PATCH, GET /balances
//	                           served from the paper ledger in Flash's shapes
//
// So the engine, referee, signer, copy engine and order lifecycle run the
// exact code they run with real money; only the money is pretend.
package paper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/atomicfile"
	"github.com/vikram/agent-arena/internal/evm"
	"github.com/vikram/agent-arena/internal/flash"
)

// Options configure a Venue.
type Options struct {
	Real     http.RoundTripper // nil = http.DefaultTransport
	BaseURL  string            // real Flash base, e.g. https://flash.definitive.fi/v1
	APIKey   string            // used only for the venue's own exit-price quotes
	FeeBps   string
	Chain    string
	USDC     string
	WETH     string
	SeedUSDC float64                    // mock USDC a wallet gets the first time it is seen
	Price    func(token string) float64 // reference USD price of a token (address)
	Dir      string                     // where venue.json is kept ("" = memory only)
	Log      *slog.Logger
}

type Venue struct {
	o        Options
	basePath string

	mu     sync.Mutex
	st     state
	quotes map[string]cachedQuote
	reset  bool // the ledger was damaged and started fresh this run
}

type state struct {
	Balances map[string]map[string]float64 `json:"balances"` // lower(address) -> lower(token) -> amount
	Seeded   map[string]bool               `json:"seeded"`
	Orders   map[string]*order             `json:"orders"`
}

type order struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`  // market | limit | bracket
	Token        string    `json:"token"` // target asset address (empty = WETH, older ledgers)
	Side         string    `json:"side"`
	Status       string    `json:"status"`
	CloseReason  string    `json:"closeReason,omitempty"`
	Funder       string    `json:"funder"`
	Qty          float64   `json:"qty"`       // spent: USDC on buy, WETH on sell
	ExpectOut    float64   `json:"expectOut"` // the quote's to.amount (net of every cost)
	Mark         float64   `json:"mark"`      // quote's WETH valuation
	LimitCross   float64   `json:"limitCross,omitempty"`
	Trigger      float64   `json:"trigger,omitempty"`   // stop (buy, upper), stop-loss (lower), take-profit (upper)
	Trail        float64   `json:"trail,omitempty"`     // trailing-stop fraction
	Watermark    float64   `json:"watermark,omitempty"` // trailing-stop high-water mark
	TwapN        int       `json:"twapN,omitempty"`     // twap slices
	TwapSec      int       `json:"twapSec,omitempty"`   // twap duration
	TwapDone     int       `json:"twapDone,omitempty"`  // slices filled
	TP           float64   `json:"tp,omitempty"`
	SL           float64   `json:"sl,omitempty"`
	ParentID     string    `json:"parentId,omitempty"`
	BracketID    string    `json:"bracketId,omitempty"`
	BracketState string    `json:"bracketState,omitempty"` // pending_activation | active | never_activated
	BracketTP    float64   `json:"bracketTp,omitempty"`
	BracketSL    float64   `json:"bracketSl,omitempty"`
	SignedMax    float64   `json:"signedMax,omitempty"`
	FilledTarget float64   `json:"filledTarget,omitempty"`
	FilledContra float64   `json:"filledContra,omitempty"`
	AvgPrice     float64   `json:"avgPrice,omitempty"`
	Fills        []fill    `json:"fills,omitempty"`
	PlacedAt     time.Time `json:"placedAt"`
	ClosedAt     time.Time `json:"closedAt,omitempty"`
}

type fill struct {
	TxID   string    `json:"txId"`
	Target float64   `json:"target"`
	Contra float64   `json:"contra"`
	Price  float64   `json:"price"`
	At     time.Time `json:"at"`
}

type cachedQuote struct {
	Side, Funder, OrderType, Token string
	Qty, LimitCross                float64
	ToAmount, Mark                 float64
	TypedData, BracketTD           string
	SignedMax                      float64
	At                             time.Time
}

// New builds a venue and loads its ledger from Dir.
func New(o Options) (*Venue, error) {
	if o.Real == nil {
		o.Real = http.DefaultTransport
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil {
		return nil, err
	}
	v := &Venue{o: o, basePath: strings.TrimRight(u.Path, "/"), quotes: map[string]cachedQuote{},
		st: state{Balances: map[string]map[string]float64{}, Seeded: map[string]bool{}, Orders: map[string]*order{}}}
	if o.Dir != "" {
		if err := os.MkdirAll(o.Dir, 0o700); err != nil {
			return nil, err
		}
		b, from, err := atomicfile.Read(v.file())
		switch {
		case err == nil:
			if jerr := json.Unmarshal(b, &v.st); jerr != nil {
				err = jerr
			} else if from != v.file() {
				o.Log.Warn("paper ledger restored from its backup", "file", from)
			}
		case errors.Is(err, os.ErrNotExist):
			err = nil // first run
		}
		if err != nil {
			// Paper money only: keep the damaged file for inspection and start a
			// fresh ledger instead of refusing to run.
			aside := v.file() + ".corrupt-" + time.Now().Format("20060102-150405")
			_ = os.Rename(v.file(), aside)
			o.Log.Error("paper ledger was damaged; starting a fresh one", "err", err, "movedTo", aside)
			v.reset = true
			v.st = state{Balances: map[string]map[string]float64{}, Seeded: map[string]bool{}, Orders: map[string]*order{}}
		}
	}
	return v, nil
}

func (v *Venue) file() string { return filepath.Join(v.o.Dir, "venue.json") }

// save writes the ledger atomically. Caller holds v.mu.
func (v *Venue) save() {
	if v.o.Dir == "" {
		return
	}
	b, _ := json.MarshalIndent(v.st, "", " ")
	if err := atomicfile.Write(v.file(), b, 0o600); err != nil {
		v.o.Log.Warn("paper ledger save failed", "err", err)
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func txID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "paper:" + hex.EncodeToString(b)
}

func num(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// --- ledger ------------------------------------------------------------------

func (v *Venue) bal(addr, token string) float64 {
	addr = lower(addr)
	if !v.st.Seeded[addr] {
		v.st.Seeded[addr] = true
		v.add(addr, v.o.USDC, v.o.SeedUSDC)
	}
	return v.st.Balances[addr][lower(token)]
}

func (v *Venue) add(addr, token string, amt float64) {
	addr = lower(addr)
	if v.st.Balances[addr] == nil {
		v.st.Balances[addr] = map[string]float64{}
	}
	nv := v.st.Balances[addr][lower(token)] + amt
	if math.Abs(nv) < 1e-12 {
		nv = 0
	}
	v.st.Balances[addr][lower(token)] = nv
}

// Balance returns a wallet's paper balance of a token.
func (v *Venue) Balance(addr, token string) float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.bal(addr, token)
}

// Deposit adds paper money to a wallet (operator "top up").
func (v *Venue) Deposit(addr, token string, amt float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bal(addr, token)
	v.add(addr, token, amt)
	v.save()
}

// Withdraw moves paper money out of a wallet (to its owner's payout address).
func (v *Venue) Withdraw(addr, token string, amt float64) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if have := v.bal(addr, token); amt <= 0 || amt > have+1e-12 {
		return "", fmt.Errorf("paper balance %.8f, cannot withdraw %.8f", have, amt)
	}
	v.add(addr, token, -amt)
	v.save()
	return txID(), nil
}

// --- transport -----------------------------------------------------------------

// RoundTrip implements http.RoundTripper.
func (v *Venue) RoundTrip(r *http.Request) (*http.Response, error) {
	p := strings.TrimPrefix(r.URL.Path, v.basePath)
	switch {
	case r.Method == http.MethodPost && p == "/quote":
		return v.forwardQuote(r)
	case r.Method == http.MethodGet && p == "/search":
		return v.o.Real.RoundTrip(r)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/balances/"):
		return v.balances(r, strings.TrimPrefix(p, "/balances/"))
	case r.Method == http.MethodPost && p == "/order":
		return v.submit(r)
	case r.Method == http.MethodGet && p == "/orders":
		return v.list(r)
	case r.Method == http.MethodPost && p == "/orders/cancel":
		return v.cancelBatch(r)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/orders/") && strings.HasSuffix(p, "/cancel"):
		return v.cancel(r, strings.TrimSuffix(strings.TrimPrefix(p, "/orders/"), "/cancel"))
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/orders/"):
		return v.update(r, strings.TrimPrefix(p, "/orders/"))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/orders/"):
		return v.get(r, strings.TrimPrefix(p, "/orders/"))
	}
	return reply(r, 404, errBody("NOT_FOUND", "paper venue: unknown endpoint "+r.Method+" "+p)), nil
}

func reply(r *http.Request, code int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)), Request: r}
}

func errBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": msg}}
}

func readBody(r *http.Request, out any) error {
	if r.Body == nil {
		return fmt.Errorf("empty body")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body.Close()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// forwardQuote passes the quote to the real Flash API and remembers what it
// said, so the order can be checked against it.
func (v *Venue) forwardQuote(r *http.Request) (*http.Response, error) {
	var reqBody []byte
	if r.Body != nil {
		reqBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		r.ContentLength = int64(len(reqBody))
	}
	resp, err := v.o.Real.RoundTrip(r)
	if err != nil || resp.StatusCode != 200 {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var req flash.QuoteRequest
	var q flash.QuoteResponse
	if json.Unmarshal(reqBody, &req) == nil && json.Unmarshal(body, &q) == nil && q.QuoteID != "" && q.To != nil {
		cq := cachedQuote{Side: req.Side, Funder: req.FunderAddress, OrderType: req.OrderType, Token: req.TargetAsset, At: time.Now(),
			ToAmount: q.To.Amount.Float()}
		cq.Qty, _ = strconv.ParseFloat(req.Qty, 64)
		cq.LimitCross, _ = strconv.ParseFloat(req.LimitCrossPrice, 64)
		if q.From != nil {
			if req.Side == "buy" && q.To.Amount.Float() > 0 {
				cq.Mark = q.To.Notional.Float() / q.To.Amount.Float()
			} else if q.From.Amount.Float() > 0 {
				cq.Mark = q.From.Notional.Float() / q.From.Amount.Float()
			}
		}
		if q.EVM != nil {
			cq.TypedData = q.EVM.OrderTypedData
		}
		if ab := q.AttachedBracket; ab != nil {
			cq.SignedMax = ab.SignedMaxFromAmount.Float()
			if ab.EVM != nil {
				cq.BracketTD = ab.EVM.OrderTypedData
			}
		}
		v.mu.Lock()
		for id, old := range v.quotes {
			if time.Since(old.At) > 10*time.Minute {
				delete(v.quotes, id)
			}
		}
		v.quotes[q.QuoteID] = cq
		v.mu.Unlock()
	}
	return resp, nil
}

// verifyTyped checks that sig is the funder's EIP-712 signature over raw.
func verifyTyped(raw, sigHex, funder string) error {
	td, err := evm.ParseTypedData(raw)
	if err != nil {
		return err
	}
	d, err := td.Digest()
	if err != nil {
		return err
	}
	return verifyDigest(d, sigHex, funder)
}

func verifyDigest(d []byte, sigHex, funder string) error {
	sig, err := evm.DecodeHex(sigHex)
	if err != nil {
		return err
	}
	who, err := evm.RecoverAddress(d, sig)
	if err != nil {
		return err
	}
	if !strings.EqualFold(who, funder) {
		return fmt.Errorf("signed by %s, not the funder %s", who, funder)
	}
	return nil
}

func (v *Venue) submit(r *http.Request) (*http.Response, error) {
	var req flash.OrderRequest
	if err := readBody(r, &req); err != nil {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "bad order body: "+err.Error())), nil
	}
	v.mu.Lock()
	cq, ok := v.quotes[req.QuoteID]
	v.mu.Unlock()
	switch {
	case !ok:
		return reply(r, 400, errBody("FAILED_PRECONDITION", "quote not found or expired; re-quote")), nil
	case time.Since(cq.At) > 5*time.Minute:
		return reply(r, 400, errBody("FAILED_PRECONDITION", "quote expired; re-quote and re-sign")), nil
	case !strings.EqualFold(req.FunderAddress, cq.Funder):
		return reply(r, 400, errBody("INVALID_ARGUMENT", "funderAddress differs from the quote")), nil
	case req.EVMOrderTypedData != cq.TypedData:
		return reply(r, 400, errBody("INVALID_ARGUMENT", "evmOrderTypedData must echo the quote's payload")), nil
	}
	if err := verifyTyped(req.EVMOrderTypedData, req.UserSignature, req.FunderAddress); err != nil {
		return reply(r, 400, errBody("INVALID_SIGNATURE", "order signature: "+err.Error())), nil
	}
	o := &order{ID: newID(), Type: cq.OrderType, Side: cq.Side, Token: cq.Token, Status: flash.StatusAccepted, Funder: req.FunderAddress,
		Qty: cq.Qty, ExpectOut: cq.ToAmount, Mark: cq.Mark, LimitCross: cq.LimitCross, PlacedAt: time.Now().UTC()}
	if ab := req.AttachedBracket; ab != nil {
		if cq.BracketTD == "" {
			return reply(r, 400, errBody("INVALID_ARGUMENT", "attachedBracket was not quoted")), nil
		}
		if err := verifyTyped(cq.BracketTD, ab.UserSignature, req.FunderAddress); err != nil {
			return reply(r, 400, errBody("INVALID_SIGNATURE", "bracket signature: "+err.Error())), nil
		}
		o.BracketState = "pending_activation"
		if ab.TakeProfit != nil {
			o.BracketTP, _ = strconv.ParseFloat(ab.TakeProfit.NotionalPrice, 64)
		}
		if ab.StopLoss != nil {
			o.BracketSL, _ = strconv.ParseFloat(ab.StopLoss.NotionalPrice, 64)
		}
		o.SignedMax, _ = strconv.ParseFloat(ab.SignedMaxFromAmount, 64)
		if o.BracketTP <= o.BracketSL {
			return reply(r, 400, errBody("INVALID_ARGUMENT", "takeProfit must be above stopLoss")), nil
		}
	}
	if msg := v.advanced(o, &req); msg != "" {
		return reply(r, 400, errBody("INVALID_ARGUMENT", msg)), nil
	}
	v.mu.Lock()
	delete(v.quotes, req.QuoteID) // a quote is consumed by its order
	v.st.Orders[o.ID] = o
	switch o.Type {
	case "market":
		v.execute(o, o.Mark)
	case "twap":
		v.twapStep(o, o.Mark) // the first slice goes at once
	}
	v.save()
	v.mu.Unlock()
	out := map[string]any{"orderId": o.ID}
	if req.AttachedBracket != nil {
		out["attachedBracket"] = map[string]any{"status": "pending_activation"}
	}
	v.o.Log.Info("paper order", "id", o.ID, "type", o.Type, "side", o.Side, "qty", o.Qty, "status", o.Status)
	return reply(r, 201, out), nil
}

// execute fills an entry or exit against the ledger. Caller holds v.mu.
func (v *Venue) execute(o *order, px float64) {
	tok := v.tok(o)
	from, to := v.o.USDC, tok
	if o.Side == "sell" {
		from, to = tok, v.o.USDC
	}
	if v.bal(o.Funder, from) < o.Qty*(1-1e-9) {
		v.close(o, flash.StatusRejected, "REASON_INSUFFICIENT_FUNDER_BALANCE")
		if o.BracketState == "pending_activation" {
			o.BracketState = "never_activated"
		}
		return
	}
	out := o.ExpectOut
	if out <= 0 { // limit fill: price it like Flash would (~0.36% + $0.06)
		if o.Side == "buy" {
			out = (o.Qty - 0.06) / px * (1 - 0.0036)
		} else {
			out = o.Qty*px*(1-0.0036) - 0.06
		}
	}
	v.add(o.Funder, from, -o.Qty)
	v.add(o.Funder, to, out)
	target, contra := out, o.Qty
	if o.Side == "sell" {
		target, contra = o.Qty, out
	}
	o.FilledTarget, o.FilledContra, o.AvgPrice = target, contra, px
	o.Fills = append(o.Fills, fill{TxID: txID(), Target: target, Contra: contra, Price: px, At: time.Now().UTC()})
	v.close(o, flash.StatusFilled, "REASON_FULLY_FILLED")
	if o.BracketState == "pending_activation" {
		qty := target
		if o.SignedMax > 0 && qty > o.SignedMax {
			qty = o.SignedMax
		}
		b := &order{ID: newID(), Type: "bracket", Side: "sell", Token: o.Token, Status: flash.StatusAccepted, Funder: o.Funder,
			Qty: qty, TP: o.BracketTP, SL: o.BracketSL, ParentID: o.ID, PlacedAt: time.Now().UTC()}
		v.st.Orders[b.ID] = b
		o.BracketID, o.BracketState = b.ID, "active"
	}
}

func (v *Venue) close(o *order, status, reason string) {
	o.Status, o.CloseReason, o.ClosedAt = status, reason, time.Now().UTC()
}

func terminal(s string) bool {
	return s == flash.StatusFilled || s == flash.StatusCancelled || s == flash.StatusRejected || s == flash.StatusTerminated
}

// --- the market --------------------------------------------------------------

// Run watches the onchain price: limit entries fill when crossed and
// brackets fire at their take profit or stop loss.
func (v *Venue) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		v.Tick(ctx)
	}
}

// Tick runs one market step at the current reference price.
func (v *Venue) Tick(ctx context.Context) {
	type fire struct {
		id, token string
		qty, px   float64
	}
	var fires []fire
	v.mu.Lock()
	for _, o := range v.st.Orders {
		if o.Type == "twap" && o.Status == flash.StatusPartiallyFilled {
			if px := v.o.Price(v.tok(o)); px > 0 {
				v.twapStep(o, px)
			}
			continue
		}
		if o.Status != flash.StatusAccepted {
			continue
		}
		px := v.o.Price(v.tok(o))
		if px <= 0 {
			continue
		}
		if o.Type == "trailing-stop" && px > o.Watermark {
			o.Watermark = px
		}
		sellFires := (o.Type == "bracket" && (px >= o.TP || px <= o.SL)) ||
			(o.Type == "stop-loss" && px <= o.Trigger) ||
			(o.Type == "take-profit" && px >= o.Trigger) ||
			(o.Type == "trailing-stop" && o.Watermark > 0 && px <= o.Watermark*(1-o.Trail))
		switch {
		case o.Type == "limit" && o.Side == "buy" && o.LimitCross > 0 && px <= o.LimitCross:
			o.ExpectOut = 0
			v.execute(o, px)
		case o.Type == "stop" && o.Side == "buy" && o.Trigger > 0 && px >= o.Trigger:
			o.ExpectOut = 0 // fills at the market once triggered
			v.execute(o, px)
		case o.Type == "twap":
			v.twapStep(o, px)
		case sellFires:
			held := v.bal(o.Funder, v.tok(o))
			if held < o.Qty*0.999 {
				// Flash cancels resting orders whose funder no longer holds the input.
				v.close(o, flash.StatusCancelled, "REASON_INSUFFICIENT_FUNDER_BALANCE")
				continue
			}
			fires = append(fires, fire{o.ID, v.tok(o), o.Qty, px})
		}
	}
	v.save()
	v.mu.Unlock()

	for _, f := range fires {
		out := v.exitProceeds(ctx, f.token, f.qty, f.px)
		v.mu.Lock()
		if o := v.st.Orders[f.id]; o != nil && o.Status == flash.StatusAccepted {
			o.ExpectOut = out
			v.execute(o, f.px)
			v.o.Log.Info("paper bracket fired", "id", o.ID, "price", f.px, "usdc", out, "status", o.Status)
		}
		v.save()
		v.mu.Unlock()
	}
}

// exitProceeds asks the real Flash API what selling qty WETH would return
// right now (all-in); if that fails it estimates ~0.36% + $0.06 like Flash.
func (v *Venue) exitProceeds(ctx context.Context, token string, qty, px float64) float64 {
	est := qty*px*(1-0.0036) - 0.06
	body, _ := json.Marshal(map[string]any{
		"targetChain": v.o.Chain, "contraChain": v.o.Chain, "targetAsset": token, "contraAsset": v.o.USDC,
		"side": "sell", "qty": strconv.FormatFloat(math.Floor(qty*1e12)/1e12, 'f', 12, 64), "orderType": "market",
		"maxSlippage": "0.03", "flashIntegratorFeeBps": v.o.FeeBps,
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(v.o.BaseURL, "/")+"/quote", bytes.NewReader(body))
	if err != nil {
		return est
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-definitive-api-key", v.o.APIKey)
	resp, err := v.o.Real.RoundTrip(req)
	if err != nil {
		return est
	}
	defer resp.Body.Close()
	var q flash.QuoteResponse
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&q) != nil || q.To == nil || q.To.Amount.Float() <= 0 {
		return est
	}
	return q.To.Amount.Float()
}

// --- reads -------------------------------------------------------------------

func (v *Venue) view(o *order) map[string]any {
	m := map[string]any{
		"orderId": o.ID, "orderType": o.Type, "side": o.Side, "status": o.Status, "funderAddress": o.Funder,
		"qty": num(o.Qty), "placedAt": o.PlacedAt.Format(time.RFC3339),
		"targetAsset": v.tok(o), "contraAsset": v.o.USDC,
	}
	if o.CloseReason != "" {
		m["closeReason"] = o.CloseReason
	} else {
		m["closeReason"] = nil
	}
	if !o.ClosedAt.IsZero() {
		m["closedAt"] = o.ClosedAt.Format(time.RFC3339)
	}
	if o.FilledTarget > 0 {
		m["filled"] = map[string]any{"targetAmount": num(o.FilledTarget), "contraAmount": num(o.FilledContra),
			"averagePrice": num(o.AvgPrice), "averageNotionalPrice": num(o.AvgPrice)}
	} else {
		m["filled"] = map[string]any{"targetAmount": "0", "contraAmount": "0"}
	}
	if o.BracketState != "" {
		ab := map[string]any{"status": o.BracketState,
			"takeProfit": map[string]any{"notionalPrice": num(o.BracketTP)}, "stopLoss": map[string]any{"notionalPrice": num(o.BracketSL)},
			"signedMaxFromAmount": num(o.SignedMax)}
		if o.BracketID != "" {
			ab["bracketOrderId"] = o.BracketID
		}
		m["attachedBracket"] = ab
	}
	if o.Type == "bracket" {
		m["sourceEntryOrderId"] = o.ParentID
		m["triggers"] = []map[string]any{{"notionalPrice": num(o.SL), "triggerType": "lower"}, {"notionalPrice": num(o.TP), "triggerType": "upper"}}
	}
	if o.Type == "limit" {
		m["limitCrossPrice"] = num(o.LimitCross)
	}
	switch o.Type {
	case "stop", "take-profit":
		m["triggers"] = []map[string]any{{"notionalPrice": num(o.Trigger), "triggerType": "upper"}}
	case "stop-loss":
		m["triggers"] = []map[string]any{{"notionalPrice": num(o.Trigger), "triggerType": "lower"}}
	case "trailing-stop":
		m["trailingStop"] = map[string]any{"trailPercent": num(o.Trail), "activated": true, "watermarkPrice": num(o.Watermark),
			"effectiveTriggerPrice": num(o.Watermark * (1 - o.Trail))}
	case "twap":
		m["twapBucketCount"], m["completedFillCount"] = o.TwapN, o.TwapDone
	}
	return m
}

// advanced reads the order-type fields of an order (they must match its
// quote, as Flash requires). It returns an error message, or "".
func (v *Venue) advanced(o *order, req *flash.OrderRequest) string {
	price := func(t flash.Trigger) float64 { p, _ := strconv.ParseFloat(t.NotionalPrice, 64); return p }
	switch o.Type {
	case "stop":
		if len(req.Triggers) != 1 || req.Triggers[0].TriggerType != "upper" {
			return "a stop buy needs one upper trigger"
		}
		o.Trigger = price(req.Triggers[0])
	case "stop-loss", "take-profit":
		if len(req.Triggers) != 1 {
			return o.Type + " needs one trigger"
		}
		o.Trigger = price(req.Triggers[0])
	case "bracket":
		if len(req.Triggers) != 2 {
			return "a bracket needs two triggers"
		}
		for _, t := range req.Triggers {
			if t.TriggerType == "upper" {
				o.TP = price(t)
			} else {
				o.SL = price(t)
			}
		}
		if o.TP <= o.SL {
			return "the take profit trigger must be above the stop loss"
		}
	case "trailing-stop":
		if req.TrailingStop == nil {
			return "trailing-stop needs trailingStop"
		}
		o.Trail, _ = strconv.ParseFloat(req.TrailingStop.TrailPercent, 64)
		if o.Trail < 0.005 || o.Trail > 0.5 {
			return "trailPercent must be 0.005-0.5"
		}
		o.Watermark = v.o.Price(v.tok(o))
	case "twap":
		if req.DurationSeconds < 300 || req.TwapBucketCount < 2 || req.TwapBucketCount > 2560 {
			return "twap needs durationSeconds >= 300 and 2-2560 buckets"
		}
		o.TwapN, o.TwapSec = req.TwapBucketCount, req.DurationSeconds
	}
	return ""
}

// twapStep fills every TWAP slice that is due (one at placement, then one
// per duration/buckets). The attached bracket activates on the first slice
// and grows with each one. Caller holds v.mu.
func (v *Venue) twapStep(o *order, px float64) {
	if o.TwapN < 1 || o.TwapSec <= 0 {
		return
	}
	slice := time.Duration(o.TwapSec) * time.Second / time.Duration(o.TwapN)
	due := min(1+int(time.Since(o.PlacedAt)/slice), o.TwapN)
	tok := v.tok(o)
	for o.TwapDone < due {
		usd := o.Qty / float64(o.TwapN)
		if v.bal(o.Funder, v.o.USDC) < usd*(1-1e-9) {
			if o.FilledTarget > 0 {
				v.close(o, flash.StatusCancelled, "REASON_INSUFFICIENT_FUNDER_BALANCE")
			} else {
				v.close(o, flash.StatusRejected, "REASON_INSUFFICIENT_FUNDER_BALANCE")
				if o.BracketState == "pending_activation" {
					o.BracketState = "never_activated"
				}
			}
			return
		}
		out := usd * (1 - 0.0036) / px
		v.add(o.Funder, v.o.USDC, -usd)
		v.add(o.Funder, tok, out)
		o.AvgPrice = (o.AvgPrice*float64(o.TwapDone) + px) / float64(o.TwapDone+1)
		o.FilledTarget += out
		o.FilledContra += usd
		o.Fills = append(o.Fills, fill{TxID: txID(), Target: out, Contra: usd, Price: px, At: time.Now().UTC()})
		o.TwapDone++
		switch {
		case o.BracketState == "pending_activation":
			b := &order{ID: newID(), Type: "bracket", Side: "sell", Token: o.Token, Status: flash.StatusAccepted, Funder: o.Funder,
				Qty: out, TP: o.BracketTP, SL: o.BracketSL, ParentID: o.ID, PlacedAt: time.Now().UTC()}
			v.st.Orders[b.ID] = b
			o.BracketID, o.BracketState = b.ID, "active"
		case o.BracketID != "":
			if b := v.st.Orders[o.BracketID]; b != nil && b.Status == flash.StatusAccepted {
				b.Qty += out
				if o.SignedMax > 0 && b.Qty > o.SignedMax {
					b.Qty = o.SignedMax
				}
			}
		}
	}
	if o.TwapDone >= o.TwapN {
		v.close(o, flash.StatusFilled, "REASON_FULLY_FILLED")
	} else {
		o.Status = flash.StatusPartiallyFilled
	}
}

func (v *Venue) fills(o *order) []map[string]any {
	out := []map[string]any{}
	for _, f := range o.Fills {
		out = append(out, map[string]any{"status": "filled", "orderId": o.ID, "rootOrderId": o.ID, "fillPrice": num(f.Price),
			"transactionId": f.TxID, "targetAmount": num(f.Target), "contraAmount": num(f.Contra), "filledAt": f.At.Format(time.RFC3339)})
	}
	return out
}

func (v *Venue) get(r *http.Request, id string) (*http.Response, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.st.Orders[id]
	if o == nil || !strings.EqualFold(o.Funder, r.URL.Query().Get("funderAddress")) {
		return reply(r, 404, errBody("NOT_FOUND", "order not found")), nil
	}
	return reply(r, 200, map[string]any{"order": v.view(o), "fills": v.fills(o)}), nil
}

func (v *Venue) list(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	funder := q.Get("funderAddress")
	want := map[string]bool{}
	for _, s := range strings.Split(q.Get("statuses"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	size, _ := strconv.Atoi(q.Get("pageSize"))
	if size <= 0 || size > 200 {
		size = 50
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var os []*order
	for _, o := range v.st.Orders {
		if (funder == "" || strings.EqualFold(o.Funder, funder)) && (len(want) == 0 || want[o.Status]) {
			os = append(os, o)
		}
	}
	sort.Slice(os, func(i, j int) bool { return os[i].PlacedAt.After(os[j].PlacedAt) })
	if len(os) > size {
		os = os[:size]
	}
	views := []map[string]any{}
	for _, o := range os {
		views = append(views, v.view(o))
	}
	return reply(r, 200, map[string]any{"orders": views, "nextPageToken": nil}), nil
}

func (v *Venue) balances(r *http.Request, addr string) (*http.Response, error) {
	v.mu.Lock()
	usdc := v.bal(addr, v.o.USDC)
	v.bal(addr, v.o.WETH)
	holdings := map[string]float64{}
	for tok, amt := range v.st.Balances[lower(addr)] {
		holdings[tok] = amt
	}
	v.save()
	v.mu.Unlock()
	out := []map[string]any{{"chain": v.o.Chain, "address": v.o.USDC, "symbol": "USDC", "tokenDecimals": 6, "balance": num(usdc), "notional": num(usdc), "isNative": false}}
	for tok, amt := range holdings {
		if tok == lower(v.o.USDC) {
			continue
		}
		out = append(out, map[string]any{"chain": v.o.Chain, "address": tok, "balance": num(amt), "notional": num(amt * v.o.Price(tok)), "isNative": false})
	}
	return reply(r, 200, map[string]any{"balances": out}), nil
}

// --- writes signed by the funder -----------------------------------------------

func (v *Venue) cancel(r *http.Request, id string) (*http.Response, error) {
	var in struct {
		CancelMessage string `json:"cancelMessage"`
		UserSignature string `json:"userSignature"`
	}
	if err := readBody(r, &in); err != nil {
		return reply(r, 400, errBody("INVALID_ARGUMENT", err.Error())), nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.st.Orders[id]
	if o == nil {
		return reply(r, 404, errBody("NOT_FOUND", "order not found")), nil
	}
	if in.CancelMessage != flash.CancelMessage(id) {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "cancelMessage does not match the canonical format")), nil
	}
	if err := verifyDigest(evm.PersonalMessageHash([]byte(in.CancelMessage)), in.UserSignature, o.Funder); err != nil {
		return reply(r, 400, errBody("INVALID_SIGNATURE", err.Error())), nil
	}
	switch o.Status {
	case flash.StatusCancelled:
		return reply(r, 200, map[string]any{"ok": true}), nil // idempotent
	case flash.StatusFilled:
		return reply(r, 422, errBody("FAILED_PRECONDITION", "order already filled")), nil
	case flash.StatusRejected, flash.StatusTerminated:
		return reply(r, 422, errBody("FAILED_PRECONDITION", "order is closed")), nil
	}
	v.close(o, flash.StatusCancelled, "REASON_USER_REQUESTED")
	if o.BracketState == "pending_activation" {
		o.BracketState = "never_activated"
	}
	v.save()
	return reply(r, 200, map[string]any{"ok": true}), nil
}

func (v *Venue) cancelBatch(r *http.Request) (*http.Response, error) {
	var in struct {
		OrderIDs      []string `json:"orderIds"`
		CancelMessage string   `json:"cancelMessage"`
		UserSignature string   `json:"userSignature"`
	}
	if err := readBody(r, &in); err != nil || len(in.OrderIDs) == 0 || len(in.OrderIDs) > 25 {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "orderIds must hold 1-25 ids")), nil
	}
	if in.CancelMessage != flash.BatchCancelMessage(in.OrderIDs) {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "cancelMessage does not match the canonical batch format")), nil
	}
	sig, err := evm.DecodeHex(in.UserSignature)
	if err != nil {
		return reply(r, 400, errBody("INVALID_SIGNATURE", err.Error())), nil
	}
	signer, err := evm.RecoverAddress(evm.PersonalMessageHash([]byte(in.CancelMessage)), sig)
	if err != nil {
		return reply(r, 400, errBody("INVALID_SIGNATURE", err.Error())), nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var results []map[string]any
	for _, id := range in.OrderIDs {
		o := v.st.Orders[id]
		res := map[string]any{"orderId": id, "ok": false}
		switch {
		case o == nil || !strings.EqualFold(o.Funder, signer):
			res["errorCode"] = "NOT_FOUND"
		case o.Status == flash.StatusFilled:
			res["errorCode"] = "ALREADY_FILLED"
		case o.Status == flash.StatusRejected:
			res["errorCode"] = "REJECTED"
		case o.Status == flash.StatusCancelled:
			res["ok"] = true
		default:
			v.close(o, flash.StatusCancelled, "REASON_USER_REQUESTED")
			if o.BracketState == "pending_activation" {
				o.BracketState = "never_activated"
			}
			res["ok"] = true
		}
		results = append(results, res)
	}
	v.save()
	return reply(r, 200, map[string]any{"results": results}), nil
}

func (v *Venue) update(r *http.Request, id string) (*http.Response, error) {
	var in struct {
		Triggers []struct {
			NotionalPrice string `json:"notionalPrice"`
			TriggerType   string `json:"triggerType"`
		} `json:"triggers"`
		UpdateMessage string `json:"updateMessage"`
		UserSignature string `json:"userSignature"`
	}
	if err := readBody(r, &in); err != nil {
		return reply(r, 400, errBody("INVALID_ARGUMENT", err.Error())), nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.st.Orders[id]
	if o == nil {
		return reply(r, 404, errBody("NOT_FOUND", "order not found")), nil
	}
	if !strings.HasPrefix(in.UpdateMessage, "Definitive Flash — Update Order\nOrder: "+id+"\n") {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "updateMessage does not name this order")), nil
	}
	if err := verifyDigest(evm.PersonalMessageHash([]byte(in.UpdateMessage)), in.UserSignature, o.Funder); err != nil {
		return reply(r, 400, errBody("INVALID_SIGNATURE", err.Error())), nil
	}
	if o.Type != "bracket" || terminal(o.Status) {
		return reply(r, 422, errBody("FAILED_PRECONDITION", "order is closed and can no longer be updated")), nil
	}
	tp, sl := o.TP, o.SL
	for _, t := range in.Triggers {
		p, err := strconv.ParseFloat(t.NotionalPrice, 64)
		if err != nil || p <= 0 {
			return reply(r, 400, errBody("INVALID_ARGUMENT", "bad trigger price")), nil
		}
		if t.TriggerType == "upper" {
			tp = p
		} else {
			sl = p
		}
		if !strings.Contains(in.UpdateMessage, t.NotionalPrice) {
			return reply(r, 400, errBody("INVALID_ARGUMENT", "updateMessage does not carry the new prices")), nil
		}
	}
	if tp <= sl {
		return reply(r, 400, errBody("INVALID_ARGUMENT", "take profit must stay above stop loss")), nil
	}
	o.TP, o.SL = tp, sl
	v.save()
	return reply(r, 200, map[string]any{"ok": true}), nil
}

// tok is the order's target asset (older ledgers stored none: WETH).
func (v *Venue) tok(o *order) string {
	if o.Token != "" {
		return o.Token
	}
	return v.o.WETH
}

// Advance moves every order's placement time back by d, as if d had passed
// (tests use it to finish a TWAP without waiting).
func (v *Venue) Advance(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, o := range v.st.Orders {
		o.PlacedAt = o.PlacedAt.Add(-d)
	}
}

// WasReset reports that the ledger was damaged and started fresh on load,
// so paper balances no longer match the recorded equity history.
func (v *Venue) WasReset() bool { return v.reset }

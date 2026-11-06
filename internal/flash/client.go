// Package flash is a typed REST + WebSocket client for the Definitive Flash
// API (https://flash.definitive.fi/v1). It adds per-endpoint rate limiting
// that follows the server's RateLimit-Policy, retries with backoff on 429/5xx
// (never for POST /order), timeouts and structured errors.
package flash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	base   string
	key    string
	http   *http.Client
	log    *slog.Logger
	mu     sync.Mutex
	limits map[string]*Limiter
}

func New(baseURL, apiKey string, log *slog.Logger) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		key:    apiKey,
		http:   &http.Client{Timeout: 25 * time.Second},
		log:    log,
		limits: map[string]*Limiter{},
	}
}

// defaultRPS is used until the server tells us its policy. Docs say 5/s per
// endpoint per key, but /search was observed at 2/s, so start low.
var defaultRPS = map[string]float64{"search": 1.5, "cancel-batch": 0.9}

func (c *Client) limiter(endpoint string) *Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limits[endpoint]
	if !ok {
		rps, ok := defaultRPS[endpoint]
		if !ok {
			rps = 4
		}
		l = NewLimiter(rps, 2)
		c.limits[endpoint] = l
	}
	return l
}

// adapt follows the RateLimit-Policy header with 10% headroom.
func (c *Client) adapt(endpoint string, h http.Header) {
	if rps, burst, ok := parsePolicy(h.Get("RateLimit-Policy")); ok {
		l := c.limiter(endpoint)
		want := rps * 0.9
		if b := burst - 1; b >= 1 {
			burst = b
		}
		if diff := l.Rate() - want; diff > 0.01 || diff < -0.01 {
			l.SetRate(want, burst)
		}
	}
}

func (c *Client) do(ctx context.Context, endpoint, method, path string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.limiter(endpoint).Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("x-definitive-api-key", c.key)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		start := time.Now()
		res, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if endpoint == "order" {
				// The request may have reached Flash. Never resend an order blindly.
				return fmt.Errorf("flash %s %s: %w (not retried: the order may or may not exist)", method, path, err)
			}
			if !sleepBackoff(ctx, attempt, 0) {
				return ctx.Err()
			}
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		c.adapt(endpoint, res.Header)
		c.log.Debug("flash", "method", method, "path", path, "status", res.StatusCode, "ms", time.Since(start).Milliseconds())
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			if out == nil || len(data) == 0 {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("flash %s %s: decode: %w", method, path, err)
			}
			return nil
		}
		apiErr := &APIError{HTTPStatus: res.StatusCode, Code: "HTTP_" + strconv.Itoa(res.StatusCode), Message: truncate(string(data), 300)}
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error != nil {
			env.Error.HTTPStatus = res.StatusCode
			apiErr = env.Error
		}
		lastErr = apiErr
		// Retry only what is safe: 429 always (rejected before processing),
		// 5xx except POST /order (it may have been accepted).
		retryable := res.StatusCode == 429 || (res.StatusCode >= 500 && endpoint != "order")
		if !retryable {
			return apiErr
		}
		if !sleepBackoff(ctx, attempt, retryWait(res.Header)) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("flash %s %s: retries exhausted: %w", method, path, lastErr)
}

// retryWait honours Retry-After and RateLimit-Reset (both in seconds).
func retryWait(h http.Header) time.Duration {
	best := 0
	for _, k := range []string{"Retry-After", "RateLimit-Reset"} {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get(k))); err == nil && n > best {
			best = n
		}
	}
	return time.Duration(best) * time.Second
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func sleepBackoff(ctx context.Context, attempt int, min time.Duration) bool {
	d := time.Duration(250*(1<<attempt))*time.Millisecond + time.Duration(rand.IntN(200))*time.Millisecond
	if min > d {
		d = min
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// IsCode reports whether err is a Flash API error with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// Search resolves a symbol / name / address to assets. limit <= 25.
// Some keys get 403 SEARCH_NOT_ELIGIBLE until they have enough Flash usage.
func (c *Client) Search(ctx context.Context, query, chain string, limit int) ([]Asset, error) {
	q := url.Values{"query": {query}}
	if chain != "" {
		q.Set("chain", chain)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Assets []Asset `json:"assets"`
	}
	err := c.do(ctx, "search", http.MethodGet, "/search", q, nil, &out)
	return out.Assets, err
}

func (c *Client) Quote(ctx context.Context, req *QuoteRequest) (*QuoteResponse, error) {
	var out QuoteResponse
	if err := c.do(ctx, "quote", http.MethodPost, "/quote", nil, req, &out); err != nil {
		return nil, err
	}
	if out.QuoteID == "" {
		return nil, errors.New("flash quote: empty quoteId")
	}
	return &out, nil
}

// SubmitOrder sends POST /order once. It is never retried automatically.
func (c *Client) SubmitOrder(ctx context.Context, req *OrderRequest) (*OrderResponse, error) {
	var out OrderResponse
	if err := c.do(ctx, "order", http.MethodPost, "/order", nil, req, &out); err != nil {
		return nil, err
	}
	if out.OrderID == "" {
		return nil, errors.New("flash order: empty orderId")
	}
	return &out, nil
}

// GetOrder returns the order and its fills ({"order":{...},"fills":[...]}).
func (c *Client) GetOrder(ctx context.Context, orderID, funder string) (*Order, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "orders", http.MethodGet, "/orders/"+url.PathEscape(orderID), url.Values{"funderAddress": {funder}}, nil, &raw); err != nil {
		return nil, err
	}
	var wrapped struct {
		Order *Order `json:"order"`
		Fills []Fill `json:"fills"`
	}
	var o Order
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Order != nil {
		o = *wrapped.Order
		o.Fills = wrapped.Fills
	} else if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	if o.OrderID == "" {
		return nil, errors.New("flash order: response has no orderId")
	}
	o.Raw = raw
	return &o, nil
}

// ListOrders is GET /orders (newest first). Empty funder = every funder of this key.
func (c *Client) ListOrders(ctx context.Context, funder string, statuses []string, pageSize int, pageToken string) ([]Order, string, error) {
	q := url.Values{}
	if funder != "" {
		q.Set("funderAddress", funder)
	}
	if len(statuses) > 0 {
		q.Set("statuses", strings.Join(statuses, ","))
	}
	if pageSize > 0 {
		q.Set("pageSize", strconv.Itoa(pageSize))
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	var out struct {
		Orders        []Order `json:"orders"`
		NextPageToken *string `json:"nextPageToken"`
	}
	if err := c.do(ctx, "orders-list", http.MethodGet, "/orders", q, nil, &out); err != nil {
		return nil, "", err
	}
	next := ""
	if out.NextPageToken != nil {
		next = *out.NextPageToken
	}
	return out.Orders, next, nil
}

// CancelMessage is the exact text the funder signs (EIP-191) to cancel.
func CancelMessage(orderID string) string {
	return "Definitive Flash v1 — Cancel Order\nOrder: " + orderID
}

// Cancel is idempotent on Flash's side (cancelling a cancelled order is 200);
// a terminal order returns 422 ("order already filled").
func (c *Client) Cancel(ctx context.Context, orderID, signature string) error {
	body := map[string]string{"cancelMessage": CancelMessage(orderID), "userSignature": signature}
	return c.do(ctx, "cancel", http.MethodPost, "/orders/"+url.PathEscape(orderID)+"/cancel", nil, body, nil)
}

// BatchCancelMessage is the exact EIP-191 text for POST /orders/cancel:
// header, "Orders:", one id per line, no trailing newline.
func BatchCancelMessage(orderIDs []string) string {
	return "Definitive Flash v1 — Cancel Orders\nOrders:\n" + strings.Join(orderIDs, "\n")
}

// BatchCancelResult is one entry of POST /orders/cancel (request order).
type BatchCancelResult struct {
	OrderID   string `json:"orderId"`
	OK        bool   `json:"ok"`
	ErrorCode string `json:"errorCode,omitempty"` // NOT_FOUND | ALREADY_FILLED | REJECTED | PENDING_CANCEL | SETTLING | INTERNAL_ERROR
}

// CancelBatch cancels up to 25 orders of one funder with a single signature.
// Flash answers 200 with a result per order, in request order. Ids must be
// lowercase UUIDs without duplicates.
func (c *Client) CancelBatch(ctx context.Context, orderIDs []string, signature string) ([]BatchCancelResult, error) {
	body := map[string]any{"orderIds": orderIDs, "cancelMessage": BatchCancelMessage(orderIDs), "userSignature": signature}
	var out struct {
		Results []BatchCancelResult `json:"results"`
	}
	if err := c.do(ctx, "cancel-batch", http.MethodPost, "/orders/cancel", nil, body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// SetTransport swaps the HTTP transport (paper mode routes money endpoints
// to the paper venue; quotes still reach the real API through it).
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// BracketUpdateMessage is the exact EIP-191 text for re-pricing both legs of
// a bracket order (PATCH /orders/{id}): lower leg first, em dash header, no
// trailing newline. issuedAt must be within ~1 minute of server time, and
// the decimals must be byte-identical to the request body.
func BracketUpdateMessage(orderID string, issuedAt time.Time, stopLoss, takeProfit string) string {
	return "Definitive Flash — Update Order\nOrder: " + orderID +
		"\nIssued At: " + issuedAt.UTC().Format(time.RFC3339) +
		"\nTrigger Lower Notional Price: " + stopLoss +
		"\nTrigger Upper Notional Price: " + takeProfit
}

// UpdateBracket re-prices a live bracket order (both legs are always resent).
func (c *Client) UpdateBracket(ctx context.Context, orderID, stopLoss, takeProfit, message, signature string) error {
	body := map[string]any{
		"triggers": []map[string]string{
			{"triggerType": "lower", "notionalPrice": stopLoss},
			{"triggerType": "upper", "notionalPrice": takeProfit},
		},
		"updateMessage": message,
		"userSignature": signature,
	}
	return c.do(ctx, "update", http.MethodPatch, "/orders/"+url.PathEscape(orderID), nil, body, nil)
}

func (c *Client) Balances(ctx context.Context, address string) ([]Balance, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "balances", http.MethodGet, "/balances/"+url.PathEscape(address), nil, nil, &raw); err != nil {
		return nil, err
	}
	var wrapped struct {
		Balances []Balance `json:"balances"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Balances != nil {
		return wrapped.Balances, nil
	}
	var flat []Balance
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("flash balances: %w", err)
	}
	return flat, nil
}

// BalanceOf returns the decimal balance of one token (0 if absent).
func BalanceOf(bals []Balance, chain, token string) float64 {
	for _, b := range bals {
		if strings.EqualFold(b.Address, token) && (chain == "" || b.Chain == chain) {
			return b.Balance.Float()
		}
	}
	return 0
}

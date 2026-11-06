package flash

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Dec is a decimal that Flash sends as a string ("2235.02"). It also accepts
// a JSON number or null, so a small shape change never breaks parsing.
type Dec string

func (d *Dec) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*d = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = Dec(strings.TrimSpace(s))
	default:
		*d = Dec(b)
	}
	return nil
}

// Float returns the value, or 0 when empty or unparsable.
func (d Dec) Float() float64 {
	f, _ := strconv.ParseFloat(string(d), 64)
	return f
}

func (d Dec) String() string { return string(d) }

// BracketLeg is a take-profit or stop-loss leg (exactly one of notionalPrice
// or crossPrice; limitPrice is optional).
type BracketLeg struct {
	NotionalPrice string `json:"notionalPrice,omitempty"`
	CrossPrice    string `json:"crossPrice,omitempty"`
	LimitPrice    string `json:"limitPrice,omitempty"`
}

// QuoteBracket is attachedBracket on POST /quote.
type QuoteBracket struct {
	TakeProfit *BracketLeg `json:"takeProfit"`
	StopLoss   *BracketLeg `json:"stopLoss"`
	ExpireTime string      `json:"expireTime,omitempty"`
}

// QuoteRequest is the body of POST /quote.
type QuoteRequest struct {
	TargetChain           string        `json:"targetChain"`
	ContraChain           string        `json:"contraChain"`
	TargetAsset           string        `json:"targetAsset"`
	ContraAsset           string        `json:"contraAsset"`
	Side                  string        `json:"side"` // buy | sell
	Qty                   string        `json:"qty"`  // spent asset, normalized units (contra on buy, target on sell)
	OrderType             string        `json:"orderType"`
	FunderAddress         string        `json:"funderAddress,omitempty"`
	MaxSlippage           string        `json:"maxSlippage,omitempty"` // decimal fraction, "0.03" = 3%
	LimitNotionalPrice    string        `json:"limitNotionalPrice,omitempty"`
	LimitCrossPrice       string        `json:"limitCrossPrice,omitempty"`
	AttachedBracket       *QuoteBracket `json:"attachedBracket,omitempty"`
	ExpireTime            string        `json:"expireTime,omitempty"`
	FlashIntegratorFeeBps string        `json:"flashIntegratorFeeBps,omitempty"`
	ForceMinimalAllowance bool          `json:"forceMinimalAllowance,omitempty"`
	EVMUsePermit2         *bool         `json:"evmUsePermit2,omitempty"`
	// Advanced order types (same /quote and /order endpoints):
	Triggers        []Trigger     `json:"triggers,omitempty"`        // stop, stop-loss, take-profit, bracket
	TrailingStop    *TrailingStop `json:"trailingStop,omitempty"`    // trailing-stop
	DurationSeconds int           `json:"durationSeconds,omitempty"` // twap (>= 300)
	TwapBucketCount int           `json:"twapBucketCount,omitempty"` // twap (2-2560), must match on the order
	StartTime       string        `json:"startTime,omitempty"`       // twap, must match on the order
}

// Trigger is one price trigger: upper fires at or above, lower at or below.
type Trigger struct {
	NotionalPrice string `json:"notionalPrice,omitempty"`
	CrossPrice    string `json:"crossPrice,omitempty"`
	TriggerType   string `json:"triggerType"`
	LimitPrice    string `json:"limitPrice,omitempty"`
}

// TrailingStop follows the high-water mark (sell) by TrailPercent (0.005-0.5).
type TrailingStop struct {
	TrailPercent    string `json:"trailPercent"`
	ActivationPrice string `json:"activationPrice,omitempty"`
}

// EVMTx is an onchain action Flash asks the funder to send (approve / wrap).
type EVMTx struct {
	To    string `json:"to"`
	Data  string `json:"data"`
	Value string `json:"value,omitempty"`
}

type EVMBlock struct {
	OrderTypedData  string `json:"orderTypedData"`
	PermitTypedData string `json:"permitTypedData"` // "" or null when not needed
	ApproveTx       *EVMTx `json:"approveTx"`
}

// Leg is quote.from (spent) or quote.to (received, net of fees).
// Asset is the literal "contra" or "target", not an address.
type Leg struct {
	Asset    string `json:"asset"`
	Amount   Dec    `json:"amount"`
	Notional Dec    `json:"notional"`
}

type QuoteAttachedBracket struct {
	EVM                 *EVMBlock `json:"evm"`
	Salt                string    `json:"salt"`
	Deadline            string    `json:"deadline"`
	SignedMaxFromAmount Dec       `json:"signedMaxFromAmount"`
}

type QuoteResponse struct {
	QuoteID              string                `json:"quoteId"`
	BridgeQuoteID        *string               `json:"bridgeQuoteId"`
	OrderType            string                `json:"orderType"`
	Side                 string                `json:"side"`
	From                 *Leg                  `json:"from"`
	To                   *Leg                  `json:"to"`
	Fees                 json.RawMessage       `json:"fees"`
	EstimatedPriceImpact Dec                   `json:"estimatedPriceImpact"`
	RecommendedSlippage  Dec                   `json:"recommendedSlippage"`
	Wrap                 json.RawMessage       `json:"wrap"`
	EVM                  *EVMBlock             `json:"evm"`
	AttachedBracket      *QuoteAttachedBracket `json:"attachedBracket,omitempty"`
	SetupTxs             []string              `json:"setupTxs"`
}

// NeedsWrap reports whether the quote asks for a native-asset wrap first.
func (q *QuoteResponse) NeedsWrap() bool {
	w := bytes.TrimSpace(q.Wrap)
	return len(w) > 0 && !bytes.Equal(w, []byte("null"))
}

// FeeNotional is fees.estimatedFeeNotional in USD.
func (q *QuoteResponse) FeeNotional() float64 {
	var f struct {
		EstimatedFeeNotional Dec `json:"estimatedFeeNotional"`
	}
	if len(q.Fees) == 0 || json.Unmarshal(q.Fees, &f) != nil {
		return 0
	}
	return f.EstimatedFeeNotional.Float()
}

// SubmitBracket is attachedBracket on POST /order: the legs as quoted plus
// the signature and the quote's salt / deadline / signedMaxFromAmount.
type SubmitBracket struct {
	TakeProfit          *BracketLeg `json:"takeProfit"`
	StopLoss            *BracketLeg `json:"stopLoss"`
	UserSignature       string      `json:"userSignature"`
	Deadline            string      `json:"deadline"`
	SignedMaxFromAmount string      `json:"signedMaxFromAmount"`
	Salt                string      `json:"salt,omitempty"`
}

// OrderRequest is the body of POST /order. Only fields documented for
// SubmitOrderRequest are sent (quote-only fields such as expireTime,
// evmUsePermit2 and forceMinimalAllowance are left out on purpose).
type OrderRequest struct {
	TargetChain           string         `json:"targetChain"`
	ContraChain           string         `json:"contraChain"`
	TargetAsset           string         `json:"targetAsset"`
	ContraAsset           string         `json:"contraAsset"`
	Side                  string         `json:"side"`
	Qty                   string         `json:"qty"`
	OrderType             string         `json:"orderType"`
	MaxSlippage           string         `json:"maxSlippage,omitempty"`
	LimitNotionalPrice    string         `json:"limitNotionalPrice,omitempty"`
	LimitCrossPrice       string         `json:"limitCrossPrice,omitempty"`
	FunderAddress         string         `json:"funderAddress"`
	QuoteID               string         `json:"quoteId,omitempty"`
	BridgeQuoteID         string         `json:"bridgeQuoteId,omitempty"`
	FlashIntegratorFeeBps string         `json:"flashIntegratorFeeBps,omitempty"`
	AttributionCode       string         `json:"erc8021AttributionCode,omitempty"`
	UserSignature         string         `json:"userSignature"`
	EVMOrderTypedData     string         `json:"evmOrderTypedData"`
	AttachedBracket       *SubmitBracket `json:"attachedBracket,omitempty"`
	Triggers              []Trigger      `json:"triggers,omitempty"`
	TrailingStop          *TrailingStop  `json:"trailingStop,omitempty"`
	DurationSeconds       int            `json:"durationSeconds,omitempty"`
	TwapBucketCount       int            `json:"twapBucketCount,omitempty"`
	StartTime             string         `json:"startTime,omitempty"`
}

// NewOrderRequest echoes the trade fields of a quote request into a submit body.
func NewOrderRequest(q *QuoteRequest, quoteID, entrySig, entryTypedData string) *OrderRequest {
	return &OrderRequest{
		TargetChain: q.TargetChain, ContraChain: q.ContraChain, TargetAsset: q.TargetAsset, ContraAsset: q.ContraAsset,
		Side: q.Side, Qty: q.Qty, OrderType: q.OrderType, MaxSlippage: q.MaxSlippage,
		LimitNotionalPrice: q.LimitNotionalPrice, LimitCrossPrice: q.LimitCrossPrice,
		FunderAddress: q.FunderAddress, QuoteID: quoteID, FlashIntegratorFeeBps: q.FlashIntegratorFeeBps,
		UserSignature: entrySig, EVMOrderTypedData: entryTypedData,
		Triggers: q.Triggers, TrailingStop: q.TrailingStop, DurationSeconds: q.DurationSeconds,
		TwapBucketCount: q.TwapBucketCount, StartTime: q.StartTime,
	}
}

type OrderResponse struct {
	OrderID         string          `json:"orderId"`
	AttachedBracket json.RawMessage `json:"attachedBracket,omitempty"`
}

// Filled is order.filled (REST has the averages; the stream only amounts).
type Filled struct {
	TargetAmount         Dec `json:"targetAmount"`
	ContraAmount         Dec `json:"contraAmount"`
	AveragePrice         Dec `json:"averagePrice"`
	AverageNotionalPrice Dec `json:"averageNotionalPrice"`
}

// OrderBracket is order.attachedBracket on the entry order. The protective
// order gets its own id (bracketOrderId) once it activates.
type OrderBracket struct {
	Status              string      `json:"status"` // pending_activation | active | never_activated
	BracketOrderID      string      `json:"bracketOrderId"`
	TakeProfit          *BracketLeg `json:"takeProfit"`
	StopLoss            *BracketLeg `json:"stopLoss"`
	SignedMaxFromAmount Dec         `json:"signedMaxFromAmount"`
	ExpiresAt           string      `json:"expiresAt"`
}

// Fill is one execution of an order (GET /orders/{id} -> fills).
type Fill struct {
	Status                string `json:"status"`
	OrderID               string `json:"orderId"`
	FillPrice             Dec    `json:"fillPrice"`
	TransactionID         string `json:"transactionId"`
	FeeNotional           Dec    `json:"feeNotional"`
	IntegratorFeeNotional Dec    `json:"integratorFeeNotional"`
	NetworkFeeNotional    Dec    `json:"networkFeeNotional"`
	ContraAmount          Dec    `json:"contraAmount"`
	TargetAmount          Dec    `json:"targetAmount"`
	FilledAt              string `json:"filledAt"`
}

// Order is the GET /orders/{id} view and the WebSocket "orders" update.
type Order struct {
	OrderID            string        `json:"orderId"`
	Status             string        `json:"status"`
	CloseReason        *string       `json:"closeReason"`
	OrderType          string        `json:"orderType"`
	Side               string        `json:"side"`
	Qty                Dec           `json:"qty"`
	FunderAddress      string        `json:"funderAddress"`
	Filled             *Filled       `json:"filled"`
	AttachedBracket    *OrderBracket `json:"attachedBracket,omitempty"`
	SourceEntryOrderID string        `json:"sourceEntryOrderId,omitempty"`
	PlacedAt           string        `json:"placedAt"`
	ClosedAt           string        `json:"closedAt"`

	Fills []Fill          `json:"-"` // REST only
	Raw   json.RawMessage `json:"-"`
}

// Order statuses (ORDER_STATUS_*).
const (
	StatusPending         = "ORDER_STATUS_PENDING"
	StatusAccepted        = "ORDER_STATUS_ACCEPTED"
	StatusPartiallyFilled = "ORDER_STATUS_PARTIALLY_FILLED"
	StatusFilled          = "ORDER_STATUS_FILLED"
	StatusCancelled       = "ORDER_STATUS_CANCELLED"
	StatusRejected        = "ORDER_STATUS_REJECTED"
	StatusTerminated      = "ORDER_STATUS_TERMINATED"
)

// Terminal is true only for the documented final states. Unknown statuses
// (e.g. ORDER_STATUS_UNSPECIFIED) are treated as still open, so a trade is
// never closed on a status we do not understand.
func (o *Order) Terminal() bool {
	switch o.Status {
	case StatusFilled, StatusCancelled, StatusRejected, StatusTerminated:
		return true
	}
	return false
}

// Reason returns closeReason or "".
func (o *Order) Reason() string {
	if o.CloseReason == nil {
		return ""
	}
	return *o.CloseReason
}

// FilledTarget / FilledContra are the executed amounts (0 when none).
func (o *Order) FilledTarget() float64 {
	if o.Filled == nil {
		return 0
	}
	return o.Filled.TargetAmount.Float()
}

func (o *Order) FilledContra() float64 {
	if o.Filled == nil {
		return 0
	}
	return o.Filled.ContraAmount.Float()
}

// AvgNotionalPrice is the USD price of the target asset across fills.
func (o *Order) AvgNotionalPrice() float64 {
	if o.Filled != nil {
		if p := o.Filled.AverageNotionalPrice.Float(); p > 0 {
			return p
		}
		if p := o.Filled.AveragePrice.Float(); p > 0 {
			return p
		}
	}
	if t, c := o.FilledTarget(), o.FilledContra(); t > 0 && c > 0 {
		return c / t // USDC is ~$1
	}
	return 0
}

// TxHash returns the first onchain transaction id among the fills.
func (o *Order) TxHash() string {
	for _, f := range o.Fills {
		if strings.HasPrefix(f.TransactionID, "0x") {
			return f.TransactionID
		}
	}
	return ""
}

// FeesUSD sums the fee notionals reported on the fills.
func (o *Order) FeesUSD() float64 {
	t := 0.0
	for _, f := range o.Fills {
		t += f.FeeNotional.Float() + f.IntegratorFeeNotional.Float() + f.NetworkFeeNotional.Float()
	}
	return t
}

type Balance struct {
	Chain    string `json:"chain"`
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Balance  Dec    `json:"balance"`  // normalized units, not base units
	Notional Dec    `json:"notional"` // USD
}

type Asset struct {
	Chain       string `json:"chain"`
	Address     string `json:"address"`
	Symbol      string `json:"symbol"`
	Name        string `json:"name"`
	Decimals    int    `json:"decimals"`
	Price       Dec    `json:"price"`
	Liquidity   Dec    `json:"liquidity"` // "0" means unknown, not zero
	RiskFlagged bool   `json:"riskFlagged"`
}

// APIError is Flash's error envelope {"error":{"code","message","details"}}.
type APIError struct {
	HTTPStatus int
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details"`
}

func (e *APIError) Error() string {
	return "flash " + e.Code + " (" + strconv.Itoa(e.HTTPStatus) + "): " + e.Message
}

// Error codes we react to.
const CodeSearchNotEligible = "SEARCH_NOT_ELIGIBLE"

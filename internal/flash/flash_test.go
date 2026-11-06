package flash

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePolicy(t *testing.T) {
	for in, want := range map[string]float64{"5;w=1": 5, "2;w=1": 2, "10;w=2": 5, "3": 3} {
		rps, _, ok := parsePolicy(in)
		if !ok || rps != want {
			t.Errorf("%q -> %v %v, want %v", in, rps, ok, want)
		}
	}
	if _, _, ok := parsePolicy(""); ok {
		t.Error("empty policy must not parse")
	}
}

func TestDecAcceptsStringNumberNull(t *testing.T) {
	var v struct{ A, B, C Dec }
	if err := json.Unmarshal([]byte(`{"A":"2235.02","B":12.5,"C":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.Float() != 2235.02 || v.B.Float() != 12.5 || v.C != "" {
		t.Fatalf("got %+v", v)
	}
}

func TestBracketUpdateMessageFormat(t *testing.T) {
	at := time.Date(2026, 11, 6, 1, 45, 0, 0, time.UTC)
	got := BracketUpdateMessage("ord_1", at, "2190.00", "2310.50")
	want := "Definitive Flash — Update Order\nOrder: ord_1\nIssued At: 2026-11-06T01:45:00Z\nTrigger Lower Notional Price: 2190.00\nTrigger Upper Notional Price: 2310.50"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if CancelMessage("x") != "Definitive Flash v1 — Cancel Order\nOrder: x" {
		t.Fatal("cancel message changed")
	}
}

func TestGetOrderReadsOrderAndFills(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("funderAddress") == "" {
			w.WriteHeader(400)
			return
		}
		io.WriteString(w, `{"order":{"orderId":"o1","status":"ORDER_STATUS_FILLED","closeReason":"REASON_FULLY_FILLED","filled":{"targetAmount":"0.001","contraAmount":"2.2","averagePrice":null,"averageNotionalPrice":"2200.00"},
"attachedBracket":{"status":"active","bracketOrderId":"b1","takeProfit":{"notionalPrice":"2300"},"stopLoss":{"notionalPrice":"2150"},"signedMaxFromAmount":"0.0011","expiresAt":null}},
"fills":[{"status":"FILL_STATUS_SETTLED","orderId":"o1","transactionId":"0xabc","fillPrice":"2200.00","feeNotional":"0.01","integratorFeeNotional":"0.0066","contraAmount":"2.2","targetAmount":"0.001"}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "dpka_x", slog.New(slog.NewTextHandler(io.Discard, nil)))
	o, err := c.GetOrder(context.Background(), "o1", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	if !o.Terminal() || o.Reason() != "REASON_FULLY_FILLED" || o.AvgNotionalPrice() != 2200 || o.TxHash() != "0xabc" || o.AttachedBracket.BracketOrderID != "b1" || o.FilledContra() != 2.2 {
		t.Fatalf("parsed %+v", o)
	}
	if unknown := (&Order{Status: "ORDER_STATUS_UNSPECIFIED"}); unknown.Terminal() {
		t.Fatal("unknown statuses must not count as terminal")
	}
}

func TestSubmitOrderIsNeverRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := New(srv.URL, "dpka_x", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.SubmitOrder(context.Background(), &OrderRequest{}); err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("POST /order was sent %d times", calls)
	}
}

// wsServer is a tiny RFC 6455 server: checks the handshake and the client's
// masking, acks subscriptions, then sends a ping, a fragmented snapshot and
// an update.
func wsServer(t *testing.T, subs chan<- map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Sec-WebSocket-Version") != "13" {
			w.WriteHeader(400)
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		send := func(op byte, fin bool, p []byte) {
			b0 := op
			if fin {
				b0 |= 0x80
			}
			hdr := []byte{b0}
			if len(p) < 126 {
				hdr = append(hdr, byte(len(p)))
			} else {
				hdr = append(hdr, 126, 0, 0)
				binary.BigEndian.PutUint16(hdr[2:], uint16(len(p)))
			}
			conn.Write(append(hdr, p...))
		}
		br := bufio.NewReader(rw)
		for i := 0; i < 2; i++ { // heartbeats + orders subscriptions
			payload, masked := readClientFrame(t, br)
			if !masked {
				t.Error("client frames must be masked")
			}
			var m map[string]string
			json.Unmarshal(payload, &m)
			subs <- m
		}
		send(0x1, true, []byte(`{"channel":"subscriptions","type":"ack","subscriptions":["orders","heartbeats"]}`))
		send(0x9, true, []byte("hi")) // ping: the client must answer with a pong
		snap := []byte(`{"channel":"orders","type":"snapshot","orders":[{"orderId":"o1","status":"ORDER_STATUS_ACCEPTED"}]}`)
		send(0x1, false, snap[:20])
		send(0x0, true, snap[20:])
		if payload, _ := readClientFrame(t, br); string(payload) != "hi" {
			t.Errorf("pong payload %q", payload)
		}
		send(0x1, true, []byte(`{"channel":"orders","type":"update","orders":[{"orderId":"b1","status":"ORDER_STATUS_FILLED","sourceEntryOrderId":"o1"}]}`))
		send(0x1, true, []byte(`{"type":"error","code":"UNAUTHORIZED","message":"bad key"}`))
		time.Sleep(200 * time.Millisecond)
	}))
}

func readClientFrame(t *testing.T, br *bufio.Reader) ([]byte, bool) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		t.Errorf("read frame: %v", err)
		return nil, false
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		io.ReadFull(br, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	masked := h[1]&0x80 != 0
	var mask [4]byte
	if masked {
		io.ReadFull(br, mask[:])
	}
	p := make([]byte, n)
	io.ReadFull(br, p)
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return p, masked
}

func TestStreamSubscribesAndDeliversOrders(t *testing.T) {
	subs := make(chan map[string]string, 4)
	srv := wsServer(t, subs)
	defer srv.Close()
	var mu sync.Mutex
	var got []string
	s := &Stream{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/ws", Key: "dpka_test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnOrders: func(orders []Order, snapshot bool) {
			mu.Lock()
			defer mu.Unlock()
			for _, o := range orders {
				got = append(got, o.OrderID+":"+o.Status+":"+o.SourceEntryOrderID)
			}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	for i := 0; i < 2; i++ {
		m := <-subs
		if m["type"] != "subscribe" || m["apiKey"] != "dpka_test" || (m["channel"] != "orders" && m["channel"] != "heartbeats") {
			t.Fatalf("subscribe message %v", m)
		}
	}
	select {
	case <-done: // UNAUTHORIZED stops the stream for good
	case <-ctx.Done():
		t.Fatal("stream did not stop after UNAUTHORIZED")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "o1:ORDER_STATUS_ACCEPTED:,b1:ORDER_STATUS_FILLED:o1" || s.State() != "off" {
		t.Fatalf("got %v state %s", got, s.State())
	}
}

func TestStreamQuotesRenewAfterTTL(t *testing.T) {
	ids := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		rw.Flush()
		send := func(p string) {
			hdr := []byte{0x81, byte(len(p))}
			if len(p) >= 126 {
				hdr = []byte{0x81, 126, byte(len(p) >> 8), byte(len(p))}
			}
			conn.Write(append(hdr, p...))
		}
		br := bufio.NewReader(rw)
		quoteSub := func() string {
			for i := 0; i < 6; i++ {
				payload, _ := readClientFrame(t, br)
				if payload == nil {
					return ""
				}
				var m struct {
					Channel, SubscriptionID string
					Request                 map[string]any
				}
				json.Unmarshal(payload, &m)
				if m.Channel == "quotes" {
					if m.Request["orderType"] != "market" || m.Request["qty"] != "5.00" {
						t.Errorf("quote request %v", m.Request)
					}
					return m.SubscriptionID
				}
			}
			return ""
		}
		first := quoteSub()
		ids <- first
		send(`{"channel":"subscriptions","type":"ack"}`)
		send(`{"channel":"quotes","type":"quote","subscriptionId":"` + first + `","quote":{"quoteId":"q1","from":{"amount":"5","notional":"5"},"to":{"amount":"0.0022","notional":"4.94"}}}`)
		send(`{"channel":"quotes","type":"quote_expired","subscriptionId":"` + first + `","reason":"ttl"}`)
		send(`{"channel":"heartbeats","type":"heartbeat","counter":1}`)
		ids <- quoteSub() // renewed with a new id
		send(`{"type":"error","code":"UNAUTHORIZED","message":"bye"}`)
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	s := &Stream{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/ws", Key: "dpka_test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Quote: &QuoteRequest{TargetChain: "base", ContraChain: "base", Side: "buy", Qty: "5.00", OrderType: "market"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx)
	a, b := <-ids, <-ids
	if a == "" || b == "" || a == b {
		t.Fatalf("subscription ids %q then %q", a, b)
	}
	q, at := s.LiveQuote()
	if q == nil || q.To.Amount.Float() != 0.0022 || at.IsZero() {
		t.Fatalf("live quote %+v", q)
	}
}

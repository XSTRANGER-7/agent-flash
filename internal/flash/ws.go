package flash

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Stream follows the Flash "orders" WebSocket channel (stdlib RFC 6455
// client). It subscribes to every funder of this API key plus heartbeats,
// reconnects with backoff and calls OnOrders with each snapshot / update.
// The stream is a trigger: callers fetch details (fills) over REST.
type Stream struct {
	URL      string
	Key      string
	Log      *slog.Logger
	OnOrders func(orders []Order, snapshot bool)
	// Quote, when set, is streamed on the "quotes" channel (market, same
	// chain; each session lasts ~120s and is renewed). LiveQuote reads it.
	Quote *QuoteRequest

	quoteMu   sync.Mutex
	lastQuote *QuoteResponse
	quoteAt   time.Time
	subN      int

	live    atomic.Bool
	stopped atomic.Bool // set after UNAUTHORIZED: reconnecting would not help
	lastMsg atomic.Int64
}

// Live reports whether the stream is connected and receiving frames.
func (s *Stream) Live() bool {
	return s.live.Load() && time.Since(time.Unix(0, s.lastMsg.Load())) < 10*time.Second
}

// State is "live", "connecting" or "off".
func (s *Stream) State() string {
	switch {
	case s.Live():
		return "live"
	case s.stopped.Load():
		return "off"
	default:
		return "connecting"
	}
}

// Run keeps the stream connected until ctx ends.
func (s *Stream) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil && !s.stopped.Load() {
		start := time.Now()
		err := s.session(ctx)
		s.live.Store(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		s.Log.Warn("flash stream disconnected", "err", err, "retryIn", backoff.String())
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

type wsFrame struct {
	Channel        string          `json:"channel"`
	Type           string          `json:"type"`
	Code           string          `json:"code"`
	Message        string          `json:"message"`
	Orders         []Order         `json:"orders"`
	SubscriptionID string          `json:"subscriptionId"`
	Quote          json.RawMessage `json:"quote"`
	Reason         string          `json:"reason"`
}

// LiveQuote is the latest streamed quote and when it arrived.
func (s *Stream) LiveQuote() (*QuoteResponse, time.Time) {
	s.quoteMu.Lock()
	defer s.quoteMu.Unlock()
	return s.lastQuote, s.quoteAt
}

func (s *Stream) subscribeQuote(c *wsConn) (string, error) {
	s.quoteMu.Lock()
	s.subN++
	id := fmt.Sprintf("arena-live-%d", s.subN)
	s.quoteMu.Unlock()
	msg, err := json.Marshal(map[string]any{"channel": "quotes", "type": "subscribe", "subscriptionId": id, "apiKey": s.Key, "request": s.Quote})
	if err != nil {
		return "", err
	}
	return id, c.WriteText(msg)
}

func (s *Stream) session(ctx context.Context) error {
	c, err := dialWS(ctx, s.URL)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	for _, ch := range []string{"heartbeats", "orders"} {
		msg, _ := json.Marshal(map[string]string{"channel": ch, "type": "subscribe", "apiKey": s.Key})
		if err := c.WriteText(msg); err != nil {
			return err
		}
	}
	quoteSub, nextQuote := "", time.Now()
	for {
		if s.Quote != nil && quoteSub == "" && !time.Now().Before(nextQuote) {
			if quoteSub, err = s.subscribeQuote(c); err != nil {
				return err
			}
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(15 * time.Second)) // heartbeats arrive every second
		data, err := c.ReadMessage()
		if err != nil {
			return err
		}
		s.lastMsg.Store(time.Now().UnixNano())
		var f wsFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch {
		case f.Channel == "quotes" && f.Type == "quote" && f.SubscriptionID == quoteSub:
			var q QuoteResponse
			if json.Unmarshal(f.Quote, &q) == nil && q.To != nil {
				s.quoteMu.Lock()
				s.lastQuote, s.quoteAt = &q, time.Now()
				s.quoteMu.Unlock()
			}
		case f.Channel == "quotes" && f.Type == "quote_expired" && f.SubscriptionID == quoteSub:
			// ttl (~120s) or consumed: renew at once; after an error wait a minute.
			quoteSub = ""
			if f.Reason == "error" {
				nextQuote = time.Now().Add(time.Minute)
			}
		case f.Channel == "quotes" && f.Type == "error":
			s.Log.Warn("flash quote stream error", "code", f.Code, "message", f.Message)
			quoteSub, nextQuote = "", time.Now().Add(time.Minute)
		case f.Type == "error":
			if f.Code == "UNAUTHORIZED" {
				s.stopped.Store(true)
				return fmt.Errorf("flash stream: UNAUTHORIZED (%s); not reconnecting", f.Message)
			}
			s.Log.Warn("flash stream error frame", "code", f.Code, "message", f.Message)
			if f.Code == "CONNECTION_LIMIT" || f.Code == "STREAMING_UNAVAILABLE" {
				return errors.New("flash stream: " + f.Code)
			}
		case f.Channel == "subscriptions" && f.Type == "ack":
			if !s.live.Swap(true) {
				s.Log.Info("flash stream live", "url", s.URL)
			}
		case f.Channel == "orders" && (f.Type == "snapshot" || f.Type == "update"):
			if s.OnOrders != nil && len(f.Orders) > 0 {
				s.OnOrders(f.Orders, f.Type == "snapshot")
			}
		}
	}
}

// PingStream connects, subscribes to heartbeats and waits for the ack and
// one heartbeat (used by `arena doctor`).
func PingStream(ctx context.Context, wsURL, key string) error {
	c, err := dialWS(ctx, wsURL)
	if err != nil {
		return err
	}
	defer c.Close()
	msg, _ := json.Marshal(map[string]string{"channel": "heartbeats", "type": "subscribe", "apiKey": key})
	if err := c.WriteText(msg); err != nil {
		return err
	}
	acked := false
	_ = c.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	for {
		data, err := c.ReadMessage()
		if err != nil {
			return err
		}
		var f wsFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch {
		case f.Type == "error":
			return fmt.Errorf("%s: %s", f.Code, f.Message)
		case f.Type == "ack":
			acked = true
		case f.Channel == "heartbeats" && f.Type == "heartbeat" && acked:
			return nil
		}
	}
}

// ---- minimal RFC 6455 client ----------------------------------------------

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	switch u.Scheme {
	case "wss":
		conn, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", host)
	case "ws":
		conn, err = d.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nUser-Agent: agent-arena\r\n\r\n"
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake: %w", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake: HTTP %d", res.StatusCode)
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, errors.New("websocket handshake: bad Sec-WebSocket-Accept")
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

func (c *wsConn) Close() error { return c.conn.Close() }

// WriteText sends one masked text frame (clients must mask).
func (c *wsConn) WriteText(p []byte) error { return c.write(0x1, p) }

func (c *wsConn) write(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr = append(hdr, mask...)
	out := make([]byte, len(p))
	for i := range p {
		out[i] = p[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(append(hdr, out...))
	return err
}

const wsMaxMessage = 8 << 20

// ReadMessage returns the next complete text/binary message, answering pings
// and assembling fragments.
func (c *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x8: // close
			_ = c.write(0x8, nil)
			return nil, io.EOF
		case 0x9: // ping
			if err := c.write(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2:
			msg = append(msg, payload...)
			if len(msg) > wsMaxMessage {
				return nil, errors.New("websocket: message too large")
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > wsMaxMessage {
		err = errors.New("websocket: frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

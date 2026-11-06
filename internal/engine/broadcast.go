package engine

// Signal bot: the agents' own trades and their results, posted to a Discord
// channel (webhook) and/or a Telegram chat, so people can follow an agent
// from the apps they already use. Off unless configured; tokens are never
// logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vikram/agent-arena/internal/feed"
)

type Broadcaster struct {
	DiscordWebhook string
	TelegramToken  string
	TelegramChat   string
	TelegramAPI    string // default https://api.telegram.org (tests override)
	PublicURL      string // links in posts point here
	Every          time.Duration
	client         *http.Client
}

func (b *Broadcaster) Enabled() bool {
	return b != nil && (b.DiscordWebhook != "" || (b.TelegramToken != "" && b.TelegramChat != ""))
}

// signalText turns a feed post into a bot message, or "" when the post is
// not an agent's own trade (copies, tests, plans and blocks stay in-app).
func (e *Engine) signalText(p feed.Post, base string) string {
	if p.TradeID == "" || (p.Kind != "trade_placed" && p.Kind != "trade_closed") {
		return ""
	}
	t, ok := e.Store.Trade(p.TradeID)
	if !ok || t.WalletID != t.AgentID || t.Test || t.DryRun {
		return ""
	}
	if _, isAgent := e.Agents[t.AgentID]; !isAgent {
		return ""
	}
	if strings.HasPrefix(p.Text, "Operator is exiting") {
		return ""
	}
	msg := p.Text
	if base != "" {
		msg += "\n" + strings.TrimRight(base, "/") + "/s/trade/" + t.ID
	}
	return msg
}

// RunBroadcast posts signals until ctx ends (one message every Every).
func (e *Engine) RunBroadcast(ctx context.Context, b *Broadcaster) {
	if !b.Enabled() {
		return
	}
	if b.client == nil {
		b.client = &http.Client{Timeout: 10 * time.Second}
	}
	if b.Every <= 0 {
		b.Every = 2 * time.Second
	}
	ch, unsub := e.Feed.Subscribe()
	defer unsub()
	t := time.NewTicker(b.Every)
	defer t.Stop()
	var queue []string
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-ch:
			if msg := e.signalText(p, b.PublicURL); msg != "" && len(queue) < 100 {
				queue = append(queue, msg)
			}
		case <-t.C:
			if len(queue) == 0 {
				continue
			}
			msg := queue[0]
			queue = queue[1:]
			if err := b.send(ctx, msg); err != nil {
				e.Log.Warn("signal bot post failed", "err", redact(err.Error(), b.TelegramToken))
			}
		}
	}
}

func (b *Broadcaster) send(ctx context.Context, msg string) error {
	var firstErr error
	post := func(url string, body any) {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			firstErr = err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := b.client.Do(req)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		res.Body.Close()
		if res.StatusCode >= 300 && firstErr == nil {
			firstErr = &httpErr{res.StatusCode}
		}
	}
	if b.DiscordWebhook != "" {
		post(b.DiscordWebhook, map[string]any{"content": msg, "username": "Agent Arena"})
	}
	if b.TelegramToken != "" && b.TelegramChat != "" {
		api := b.TelegramAPI
		if api == "" {
			api = "https://api.telegram.org"
		}
		post(api+"/bot"+b.TelegramToken+"/sendMessage", map[string]any{"chat_id": b.TelegramChat, "text": msg})
	}
	return firstErr
}

type httpErr struct{ code int }

func (h *httpErr) Error() string { return "HTTP " + http.StatusText(h.code) }

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[token]")
}

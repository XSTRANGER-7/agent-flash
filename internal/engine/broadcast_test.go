package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignalBotPostsAgentTradesToDiscordAndTelegram(t *testing.T) {
	e, _ := setup(t)
	var mu sync.Mutex
	var discord, telegram []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/discord":
			discord = append(discord, m["content"].(string))
		case strings.HasPrefix(r.URL.Path, "/botSECRET/sendMessage"):
			if m["chat_id"] != "-100123" {
				t.Errorf("chat id %v", m["chat_id"])
			}
			telegram = append(telegram, m["text"].(string))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	bot := &Broadcaster{DiscordWebhook: srv.URL + "/discord", TelegramToken: "SECRET", TelegramChat: "-100123", TelegramAPI: srv.URL,
		PublicURL: "https://arena.example", Every: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunBroadcast(ctx, bot)
	time.Sleep(50 * time.Millisecond)
	res := e.Propose(context.Background(), intent()) // an agent's own trade
	if res.Trade == nil {
		t.Fatalf("propose: %+v", res)
	}
	test := intent()
	test.WalletID, test.Test = "follower1", true // test trades are not signals
	e.Cfg.TestTradeUSD, e.Cfg.TestMaxCostPct = 1, 12
	e.Propose(context.Background(), test)
	for i := 0; i < 100; i++ {
		mu.Lock()
		n := len(discord) + len(telegram)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(discord) != 1 || len(telegram) != 1 {
		t.Fatalf("discord %d telegram %d messages: %v %v", len(discord), len(telegram), discord, telegram)
	}
	if !strings.Contains(discord[0], "TrendHunter") || !strings.Contains(discord[0], "https://arena.example/s/trade/"+res.Trade.ID) {
		t.Fatalf("message: %q", discord[0])
	}
}

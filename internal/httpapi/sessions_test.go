package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vikram/agent-arena/internal/config"
	"github.com/vikram/agent-arena/internal/engine"
)

// Wallet sign-ins survive a restart, and the saved file never holds a token.
func TestSessionsSurviveRestartAndStoreNoTokens(t *testing.T) {
	dir := t.TempDir()
	s := &Server{E: &engine.Engine{Cfg: &config.Config{DataDir: dir}}}
	tok := "u_" + randHex(32)
	users.Lock()
	users.sessions = map[string]userSession{sessKey(tok): {Addr: "0x000000000000000000000000000000000000dEaD", Exp: time.Now().Add(time.Hour)}}
	s.saveSessionsLocked()
	users.sessions = map[string]userSession{} // "restart"
	users.Unlock()
	s.loadSessions()
	users.Lock()
	got, ok := users.sessions[sessKey(tok)]
	users.Unlock()
	if !ok || got.Addr != "0x000000000000000000000000000000000000dEaD" {
		t.Fatalf("session lost on restart: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), tok[2:]) {
		t.Fatal("the sessions file holds a usable token")
	}
}

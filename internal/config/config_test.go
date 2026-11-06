package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nARENA_T_A=from_file\r\nARENA_T_B=\"quoted value\"\nexport ARENA_T_C=x # trailing\nARENA_T_D='single'\n\nnot a line\n"), 0o600)
	t.Setenv("ARENA_T_A", "from_env")
	for _, k := range []string{"ARENA_T_B", "ARENA_T_C", "ARENA_T_D"} {
		os.Unsetenv(k)
		defer os.Unsetenv(k)
	}
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ARENA_T_A": "from_env", "ARENA_T_B": "quoted value", "ARENA_T_C": "x", "ARENA_T_D": "single"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal("a missing .env is fine")
	}
}

func TestStreamURL(t *testing.T) {
	if got := streamURL("https://flash.definitive.fi/v1"); got != "wss://flash.definitive.fi/v1/ws" {
		t.Fatal(got)
	}
}

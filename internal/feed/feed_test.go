package feed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Posts survive a restart, a half-written last line is skipped and does not
// spoil the next post, and history pages go back in order.
func TestFeedIsSavedAndPaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.jsonl")
	f, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		kind := "trade_placed"
		if i%2 == 1 {
			kind = "trade_blocked"
		}
		f.Publish(Post{Kind: kind, Text: "post " + string(rune('a'+i))})
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// a crash in the middle of a write
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = fh.WriteString(`{"id":"post_half","kind":"trade_pla`)
	fh.Close()

	g, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if got := g.Latest(10); len(got) != 5 || got[0].Text != "post e" || got[4].Text != "post a" {
		t.Fatalf("after restart: %+v", got)
	}
	g.Publish(Post{Kind: "trade_closed", Text: "post f"})
	g.Close()
	h, _ := Open(path, 100)
	defer h.Close()
	if got := h.Latest(1); len(got) != 1 || got[0].Text != "post f" {
		t.Fatalf("the post after a half line was lost: %+v", got)
	}

	page1, more, total := h.Page("", 2, nil)
	if len(page1) != 2 || !more || total != 6 || page1[0].Text != "post f" {
		t.Fatalf("page 1: %v more=%v total=%d", texts(page1), more, total)
	}
	page2, more, _ := h.Page(page1[1].ID, 10, nil)
	if len(page2) != 4 || more || page2[0].Text != "post d" {
		t.Fatalf("page 2: %v more=%v", texts(page2), more)
	}
	blocked, _, n := h.Page("", 10, func(p Post) bool { return strings.HasSuffix(p.Kind, "blocked") })
	if n != 2 || len(blocked) != 2 {
		t.Fatalf("filter: %v total=%d", texts(blocked), n)
	}
	if none, more, _ := h.Page("post_gone", 10, nil); len(none) != 0 || more {
		t.Fatal("an unknown cursor must return nothing")
	}
}

// Only the newest max posts are kept in memory.
func TestFeedKeepsNewestMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.jsonl")
	f, _ := Open(path, 3)
	for i := 0; i < 5; i++ {
		f.Publish(Post{Kind: "x", Text: string(rune('a' + i))})
	}
	f.Close()
	g, _ := Open(path, 3)
	defer g.Close()
	if got := texts(g.Latest(10)); strings.Join(got, "") != "edc" {
		t.Fatalf("kept %v", got)
	}
}

func texts(ps []Post) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Text
	}
	return out
}

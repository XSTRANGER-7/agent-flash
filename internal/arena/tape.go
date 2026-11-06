package arena

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vikram/agent-arena/internal/atomicfile"
)

// Tape keeps timestamped prices per token for charts: every sample for the
// last 6 hours, then one point a minute for 7 days. It is saved to disk, so a
// restart keeps the history of trades that are still open.
type Tape struct {
	path   string
	mu     sync.Mutex
	fine   map[string][]TapePoint
	coarse map[string][]TapePoint
	dirty  bool
}

// TapePoint is one price at one time (unix milliseconds).
type TapePoint struct {
	T int64   `json:"t"`
	P float64 `json:"p"`
}

const (
	tapeFine   = 6 * time.Hour
	tapeCoarse = 7 * 24 * time.Hour
	tapeStep   = time.Minute
)

type tapeFile struct {
	Fine   map[string][]TapePoint `json:"fine"`
	Coarse map[string][]TapePoint `json:"coarse"`
}

// NewTape loads path if it exists (a missing or damaged file starts empty).
func NewTape(path string) *Tape {
	t := &Tape{path: path, fine: map[string][]TapePoint{}, coarse: map[string][]TapePoint{}}
	if path == "" {
		return t
	}
	if b, _, err := atomicfile.Read(path); err == nil {
		var f tapeFile
		if json.Unmarshal(b, &f) == nil {
			if f.Fine != nil {
				t.fine = f.Fine
			}
			if f.Coarse != nil {
				t.coarse = f.Coarse
			}
		}
	}
	return t
}

// Add records a price. Points out of order or not positive are ignored.
func (t *Tape) Add(sym string, at time.Time, px float64) {
	if px <= 0 {
		return
	}
	sym = strings.ToUpper(sym)
	ms := at.UnixMilli()
	t.mu.Lock()
	defer t.mu.Unlock()
	fine := t.fine[sym]
	if n := len(fine); n > 0 && fine[n-1].T >= ms {
		return
	}
	fine = append(fine, TapePoint{ms, px})
	cut := ms - tapeFine.Milliseconds()
	i := sort.Search(len(fine), func(i int) bool { return fine[i].T >= cut })
	t.fine[sym] = fine[i:]

	coarse := t.coarse[sym]
	if n := len(coarse); n == 0 || ms-coarse[n-1].T >= tapeStep.Milliseconds() {
		coarse = append(coarse, TapePoint{ms, px})
	}
	cut = ms - tapeCoarse.Milliseconds()
	i = sort.Search(len(coarse), func(i int) bool { return coarse[i].T >= cut })
	t.coarse[sym] = coarse[i:]
	t.dirty = true
}

// Range returns the prices between from and to: fine points where the fine
// window reaches, one a minute before that, thinned to at most max points
// (the last point is always kept).
func (t *Tape) Range(sym string, from, to time.Time, max int) []TapePoint {
	sym = strings.ToUpper(sym)
	a, b := from.UnixMilli(), to.UnixMilli()
	t.mu.Lock()
	fine, coarse := t.fine[sym], t.coarse[sym]
	fineStart := int64(1<<62 - 1)
	if len(fine) > 0 {
		fineStart = fine[0].T
	}
	var out []TapePoint
	for _, p := range coarse {
		if p.T >= a && p.T <= b && p.T < fineStart {
			out = append(out, p)
		}
	}
	for _, p := range fine {
		if p.T >= a && p.T <= b {
			out = append(out, p)
		}
	}
	t.mu.Unlock()
	if max > 1 && len(out) > max {
		step := float64(len(out)-1) / float64(max-1)
		thin := make([]TapePoint, 0, max)
		for i := 0; i < max; i++ {
			thin = append(thin, out[int(float64(i)*step+0.5)])
		}
		thin[len(thin)-1] = out[len(out)-1]
		out = thin
	}
	return out
}

// Save writes the tape if it changed since the last save.
func (t *Tape) Save() error {
	if t.path == "" {
		return nil
	}
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(tapeFile{Fine: t.fine, Coarse: t.coarse})
	t.dirty = false
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if err := atomicfile.Write(t.path, b, 0o600); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // data dir removed (tests); nothing to keep
		}
		return err
	}
	return nil
}

// Package metrics exposes simple Prometheus-format counters (stdlib only).
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

var (
	mu       sync.Mutex
	counters = map[string]float64{}
	gauges   = map[string]float64{}
)

func Inc(name string)            { Add(name, 1) }
func Add(name string, v float64) { mu.Lock(); counters[name] += v; mu.Unlock() }
func Set(name string, v float64) { mu.Lock(); gauges[name] = v; mu.Unlock() }

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		write := func(kind string, m map[string]float64) {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "# TYPE arena_%s %s\narena_%s %g\n", k, kind, k, m[k])
			}
		}
		write("counter", counters)
		write("gauge", gauges)
	})
}

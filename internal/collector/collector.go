// Package collector holds the pluggable crawl sources.
//
// Adding a source means adding one Go file here that calls Register in an
// init function. The registry is synced into the sources table at startup, so
// no SQL seed data has to be maintained alongside the code.
package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/BenLocal/exams/internal/model"
)

// Collector is a crawl source.
//
// List must only fetch the source's list pages. Fetching every detail page on
// every run multiplies the request count by the number of announcements and is
// the fastest way to get a crawler blocked.
type Collector interface {
	// Key is the stable identifier stored in the database. Never change it
	// for an existing source: it is the foreign key for every scraped row.
	Key() string
	// Name is the human-readable label shown in the UI.
	Name() string
	// BaseURL is the site root, used for resolving relative links and shown
	// on the sources page.
	BaseURL() string
	// List returns the announcements visible from the list pages.
	List(ctx context.Context, f *Fetcher) ([]model.Item, error)
}

// Detailer is implemented by collectors that need a second request per
// announcement to obtain the body text.
//
// The runner calls Detail only for items that are new or whose list-level
// fields changed, so a steady-state crawl costs one request per source.
type Detailer interface {
	Detail(ctx context.Context, f *Fetcher, it model.Item) (model.Item, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]Collector{}
)

// Register adds a collector to the global registry. It panics on a duplicate
// key, which is a programming error worth failing loudly at startup.
func Register(c Collector) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[c.Key()]; dup {
		panic(fmt.Sprintf("collector: duplicate registration for key %q", c.Key()))
	}
	registry[c.Key()] = c
}

// Get returns a registered collector by key.
func Get(key string) (Collector, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	c, ok := registry[key]
	return c, ok
}

// All returns every registered collector, ordered by key so that startup is
// deterministic.
func All() []Collector {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Collector, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Keys returns every registered collector key.
func Keys() []string {
	all := All()
	out := make([]string, 0, len(all))
	for _, c := range all {
		out = append(out, c.Key())
	}
	return out
}

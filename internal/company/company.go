// Package company resolves the organisation that owns a netblock.
//
// ipapi.is derives company_name from netblock-level whois, not from the ASN.
// This package reproduces that by querying the responsible RIR over RDAP and
// caching the answer for the whole range the registry reports, so a single
// lookup of 32.5.140.2 also answers every other address in 32.5.0.0/16.
package company

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ipapi/internal/ipx"
)

const (
	sourceRDAP  = "rdap"
	sourceEmpty = "rdap_no_registrant"

	positiveTTL = 90 * 24 * time.Hour
	negativeTTL = 6 * time.Hour
)

// Record is one cached netblock -> organisation mapping.
type Record struct {
	Name    string `json:"name"`
	Country string `json:"country,omitempty"`
	Netname string `json:"netname,omitempty"`
	Source  string `json:"source"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Fetched int64  `json:"fetched"`
}

func (r Record) expired() bool {
	ttl := positiveTTL
	if r.Name == "" {
		ttl = negativeTTL
	}
	return time.Since(time.Unix(r.Fetched, 0)) > ttl
}

type entry struct {
	start ipx.Key
	end   ipx.Key
	rec   Record
}

// Resolver holds the on-disk range cache and talks to the RIRs on a miss.
type Resolver struct {
	mu      sync.RWMutex
	entries []entry // sorted by start key
	dirty   bool

	cacheDir string
	file     string
	http     *http.Client
	boot     atomic.Pointer[bootstrap]

	asnMu    sync.RWMutex
	asnCache map[uint32]Record
	asnDirty bool
	asnFile  string
	asnBoot  atomic.Pointer[[]asnRange]

	rateMu      sync.Mutex
	nextAt      time.Time
	minInterval time.Duration
	userAgent   string

	flightMu sync.Mutex
	flight   map[string]chan struct{}

	log    *slog.Logger
	stop   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type Options struct {
	CacheDir    string
	Timeout     time.Duration
	MinInterval time.Duration
	UserAgent   string
	Logger      *slog.Logger
}

func New(opt Options) (*Resolver, error) {
	if opt.Timeout == 0 {
		opt.Timeout = 6 * time.Second
	}
	if opt.MinInterval == 0 {
		opt.MinInterval = 250 * time.Millisecond
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "ipapi-selfhosted/0.1"
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if err := os.MkdirAll(opt.CacheDir, 0o755); err != nil {
		return nil, err
	}
	r := &Resolver{
		cacheDir:    opt.CacheDir,
		file:        filepath.Join(opt.CacheDir, "company.json"),
		asnFile:     filepath.Join(opt.CacheDir, "asn-org.json"),
		asnCache:    map[uint32]Record{},
		http:        &http.Client{Timeout: opt.Timeout},
		minInterval: opt.MinInterval,
		userAgent:   opt.UserAgent,
		flight:      map[string]chan struct{}{},
		log:         opt.Logger,
		stop:        make(chan struct{}),
	}
	// Cancelled by Close so background work never outlives the resolver.
	bgCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	if err := r.load(); err != nil {
		return nil, err
	}
	if err := r.loadASN(); err != nil {
		return nil, err
	}

	// The bootstrap is fetched in the background: it costs a network round trip
	// on a host with no cached copy, and blocking here would delay the HTTP
	// listener coming up. Until it lands, lookups go through the public
	// redirector, which resolves to the same registries.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()
		if bs := r.loadBootstrap(ctx); bs != nil && (len(bs.v4) > 0 || len(bs.v6) > 0) {
			r.boot.Store(bs)
			r.log.Info("rdap bootstrap loaded", "v4_prefixes", len(bs.v4), "v6_prefixes", len(bs.v6))
		} else {
			r.log.Warn("rdap bootstrap unavailable, using redirector", "redirector", rdapRedirector)
		}
		r.loadASNBootstrap(ctx)
	}()

	r.wg.Add(1)
	go r.flushLoop()
	return r, nil
}

// Len reports how many netblocks are currently cached.
func (r *Resolver) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Lookup returns the organisation owning addr. It answers from cache when
// possible and otherwise queries the registry; a stale entry is served if the
// refresh fails, and ok is false when nothing could be determined.
func (r *Resolver) Lookup(ctx context.Context, addr netip.Addr) (Record, bool) {
	key := ipx.KeyOf(addr)
	if rec, found := r.find(key); found && !rec.expired() {
		return rec, rec.Name != ""
	}

	rec, err := r.fetchOnce(ctx, addr)
	if err != nil {
		r.log.Debug("rdap lookup failed", "ip", addr.String(), "err", err)
		// Fall back to a stale entry rather than returning nothing.
		if stale, found := r.find(key); found {
			return stale, stale.Name != ""
		}
		return Record{}, false
	}
	return rec, rec.Name != ""
}

// singleflight collapses concurrent misses for the same key into one outbound
// request. Waiters re-read the cache via reread once the leader finishes.
func (r *Resolver) singleflight(
	ctx context.Context,
	key string,
	do func() (Record, error),
	reread func() (Record, bool),
) (Record, error) {
	r.flightMu.Lock()
	if ch, ok := r.flight[key]; ok {
		r.flightMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
		if rec, found := reread(); found {
			return rec, nil
		}
		return Record{}, errNoResult
	}
	ch := make(chan struct{})
	r.flight[key] = ch
	r.flightMu.Unlock()

	defer func() {
		r.flightMu.Lock()
		delete(r.flight, key)
		r.flightMu.Unlock()
		close(ch)
	}()

	return do()
}

// fetchOnce resolves the netblock owning addr, de-duplicating concurrent
// lookups of the same address.
func (r *Resolver) fetchOnce(ctx context.Context, addr netip.Addr) (Record, error) {
	return r.singleflight(ctx, addr.String(), func() (Record, error) {
		rec, err := r.fetch(ctx, addr)
		if err != nil {
			return Record{}, err
		}
		r.insert(rec)
		return rec, nil
	}, func() (Record, bool) { return r.find(ipx.KeyOf(addr)) })
}

// find returns the most specific cached range containing key.
func (r *Resolver) find(key ipx.Key) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Index of the first entry starting after key.
	i := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].start.Compare(key) > 0
	})

	var best Record
	var bestSize uint64
	found := false
	// Ranges may nest, so walk back over the candidates that start at or
	// before key and keep the smallest one that still contains it.
	for j := i - 1; j >= 0; j-- {
		e := r.entries[j]
		if e.end.Compare(key) >= 0 {
			if size := ipx.Size(e.start, e.end); !found || size < bestSize {
				best, bestSize, found = e.rec, size, true
			}
		}
		if i-j > 64 {
			break
		}
	}
	return best, found
}

func (r *Resolver) insert(rec Record) {
	start, err1 := netip.ParseAddr(rec.Start)
	end, err2 := netip.ParseAddr(rec.End)
	if err1 != nil || err2 != nil {
		return
	}
	e := entry{start: ipx.KeyOf(start), end: ipx.KeyOf(end), rec: rec}

	r.mu.Lock()
	defer r.mu.Unlock()
	i := sort.Search(len(r.entries), func(i int) bool {
		return r.entries[i].start.Compare(e.start) >= 0
	})
	// Replace an existing entry for exactly the same range.
	for j := i; j < len(r.entries) && r.entries[j].start.Compare(e.start) == 0; j++ {
		if r.entries[j].end.Compare(e.end) == 0 {
			r.entries[j] = e
			r.dirty = true
			return
		}
	}
	r.entries = append(r.entries, entry{})
	copy(r.entries[i+1:], r.entries[i:])
	r.entries[i] = e
	r.dirty = true
}

func (r *Resolver) load() error {
	b, err := os.ReadFile(r.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var recs []Record
	if err := json.Unmarshal(b, &recs); err != nil {
		r.log.Warn("company cache unreadable, starting empty", "file", r.file, "err", err)
		return nil
	}
	for _, rec := range recs {
		r.insert(rec)
	}
	r.dirty = false
	r.log.Info("company cache loaded", "netblocks", len(r.entries), "file", r.file)
	return nil
}

// Flush writes the cache to disk if it changed since the last write.
func (r *Resolver) Flush() error {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	recs := make([]Record, 0, len(r.entries))
	for _, e := range r.entries {
		recs = append(recs, e.rec)
	}
	r.dirty = false
	r.mu.Unlock()

	b, err := json.MarshalIndent(recs, "", " ")
	if err != nil {
		return err
	}
	tmp := r.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.file)
}

func (r *Resolver) flushLoop() {
	defer r.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := r.Flush(); err != nil {
				r.log.Warn("company cache flush failed", "err", err)
			}
			if err := r.FlushASN(); err != nil {
				r.log.Warn("asn cache flush failed", "err", err)
			}
		case <-r.stop:
			return
		}
	}
}

// Close stops background work and persists the cache.
func (r *Resolver) Close() error {
	r.cancel()
	close(r.stop)
	r.wg.Wait()
	err := r.Flush()
	if aerr := r.FlushASN(); err == nil {
		err = aerr
	}
	return err
}

var errNoResult = errNoResultType{}

type errNoResultType struct{}

func (errNoResultType) Error() string { return "company: no result" }

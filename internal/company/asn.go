package company

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const bootstrapASNURL = "https://data.iana.org/rdap/asn.json"

// asnRange maps a block of AS numbers to the RDAP service of its registry.
type asnRange struct {
	lo, hi uint32
	url    string
}

// parseIANAASN reads the IANA autnum bootstrap, whose service keys are AS
// number ranges ("4608-4865") or single numbers rather than CIDR prefixes.
func parseIANAASN(b []byte) ([]asnRange, error) {
	var f ianaFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	var out []asnRange
	for _, svc := range f.Services {
		if len(svc) < 2 {
			continue
		}
		var base string
		for _, u := range svc[1] {
			if strings.HasPrefix(u, "https://") {
				base = u
				break
			}
		}
		if base == "" {
			continue
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		for _, spec := range svc[0] {
			lo, hi, err := parseASNRange(spec)
			if err != nil {
				continue
			}
			out = append(out, asnRange{lo: lo, hi: hi, url: base})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("rdap asn bootstrap: no usable services")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	return out, nil
}

func parseASNRange(spec string) (uint32, uint32, error) {
	spec = strings.TrimSpace(spec)
	loStr, hiStr, ok := strings.Cut(spec, "-")
	if !ok {
		hiStr = loStr
	}
	lo, err := strconv.ParseUint(strings.TrimSpace(loStr), 10, 32)
	if err != nil {
		return 0, 0, err
	}
	hi, err := strconv.ParseUint(strings.TrimSpace(hiStr), 10, 32)
	if err != nil {
		return 0, 0, err
	}
	if hi < lo {
		return 0, 0, fmt.Errorf("asn range %q inverted", spec)
	}
	return uint32(lo), uint32(hi), nil
}

// serviceForASN returns the RDAP base URL responsible for asn.
func (r *Resolver) serviceForASN(asn uint32) string {
	ranges := r.asnBoot.Load()
	if ranges != nil {
		list := *ranges
		i := sort.Search(len(list), func(i int) bool { return list[i].hi >= asn })
		if i < len(list) && list[i].lo <= asn && asn <= list[i].hi {
			return list[i].url
		}
	}
	return rdapRedirector
}

type rdapAutnum struct {
	Handle      string       `json:"handle"`
	Name        string       `json:"name"`
	Country     string       `json:"country"`
	StartAutnum uint32       `json:"startAutnum"`
	EndAutnum   uint32       `json:"endAutnum"`
	Entities    []rdapEntity `json:"entities"`
	Remarks     []rdapRemark `json:"remarks"`
}

// fetchASN queries the responsible registry for the organisation behind asn.
// The name is taken from the registrant entity, falling back to the whois
// "descr" attribute that APNIC exposes as a remark, exactly as for netblocks.
func (r *Resolver) fetchASN(ctx context.Context, asn uint32) (Record, error) {
	url := r.serviceForASN(asn) + "autnum/" + strconv.FormatUint(uint64(asn), 10)

	r.throttle(ctx)
	body, err := r.get(ctx, url, "application/rdap+json")
	if err != nil {
		return Record{}, err
	}
	var doc rdapAutnum
	if err := json.Unmarshal(body, &doc); err != nil {
		return Record{}, err
	}

	name := registrantName(doc.Entities)
	if name == "" {
		name = remarksName(doc.Remarks)
	}
	rec := Record{
		Name:    name,
		Country: strings.ToUpper(strings.TrimSpace(doc.Country)),
		Netname: strings.TrimSpace(doc.Name), // the AS handle, e.g. ATT-INTERNET4
		Fetched: time.Now().Unix(),
	}
	if name == "" {
		rec.Source = sourceEmpty
	} else {
		rec.Source = sourceRDAP
	}
	return rec, nil
}

// LookupASN returns the organisation registered for an AS number. Unlike the
// netblock cache this is keyed by the number itself, since there are only tens
// of thousands of active ASNs and real traffic concentrates on a few hundred.
func (r *Resolver) LookupASN(ctx context.Context, asn uint32) (Record, bool) {
	if asn == 0 {
		return Record{}, false
	}
	if rec, ok := r.asnGet(asn); ok && !rec.expired() {
		return rec, rec.Name != ""
	}

	key := "as" + strconv.FormatUint(uint64(asn), 10)
	rec, err := r.singleflight(ctx, key, func() (Record, error) {
		rec, err := r.fetchASN(ctx, asn)
		if err != nil {
			return Record{}, err
		}
		r.asnPut(asn, rec)
		return rec, nil
	}, func() (Record, bool) { return r.asnGet(asn) })

	if err != nil {
		r.log.Debug("rdap autnum lookup failed", "asn", asn, "err", err)
		if stale, ok := r.asnGet(asn); ok {
			return stale, stale.Name != ""
		}
		return Record{}, false
	}
	return rec, rec.Name != ""
}

func (r *Resolver) asnGet(asn uint32) (Record, bool) {
	r.asnMu.RLock()
	defer r.asnMu.RUnlock()
	rec, ok := r.asnCache[asn]
	return rec, ok
}

func (r *Resolver) asnPut(asn uint32, rec Record) {
	r.asnMu.Lock()
	defer r.asnMu.Unlock()
	r.asnCache[asn] = rec
	r.asnDirty = true
}

// ASNLen reports how many AS numbers are cached.
func (r *Resolver) ASNLen() int {
	r.asnMu.RLock()
	defer r.asnMu.RUnlock()
	return len(r.asnCache)
}

func (r *Resolver) loadASN() error {
	b, err := os.ReadFile(r.asnFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var raw map[string]Record
	if err := json.Unmarshal(b, &raw); err != nil {
		r.log.Warn("asn cache unreadable, starting empty", "file", r.asnFile, "err", err)
		return nil
	}
	r.asnMu.Lock()
	defer r.asnMu.Unlock()
	for k, rec := range raw {
		if n, err := strconv.ParseUint(k, 10, 32); err == nil {
			r.asnCache[uint32(n)] = rec
		}
	}
	r.log.Info("asn cache loaded", "asns", len(r.asnCache), "file", r.asnFile)
	return nil
}

// FlushASN persists the AS number cache if it changed.
func (r *Resolver) FlushASN() error {
	r.asnMu.Lock()
	if !r.asnDirty {
		r.asnMu.Unlock()
		return nil
	}
	raw := make(map[string]Record, len(r.asnCache))
	for k, rec := range r.asnCache {
		raw[strconv.FormatUint(uint64(k), 10)] = rec
	}
	r.asnDirty = false
	r.asnMu.Unlock()

	b, err := json.MarshalIndent(raw, "", " ")
	if err != nil {
		return err
	}
	tmp := r.asnFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.asnFile)
}

func (r *Resolver) loadASNBootstrap(ctx context.Context) {
	path := filepath.Join(r.cacheDir, "rdap-bootstrap-asn.json")
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < bootstrapMaxAge {
		if b, err := os.ReadFile(path); err == nil {
			if rs, err := parseIANAASN(b); err == nil {
				r.asnBoot.Store(&rs)
				return
			}
		}
	}
	if b, err := r.get(ctx, bootstrapASNURL, "application/json"); err == nil {
		if rs, perr := parseIANAASN(b); perr == nil {
			_ = os.WriteFile(path, b, 0o644)
			r.asnBoot.Store(&rs)
			return
		}
	}
	if b, err := os.ReadFile(path); err == nil {
		if rs, perr := parseIANAASN(b); perr == nil {
			r.asnBoot.Store(&rs)
		}
	}
}

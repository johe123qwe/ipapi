package company

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// RDAP bootstrap: maps an address to the RDAP service of the responsible RIR.
// ---------------------------------------------------------------------------

const (
	bootstrapV4URL = "https://data.iana.org/rdap/ipv4.json"
	bootstrapV6URL = "https://data.iana.org/rdap/ipv6.json"
	// Fallback redirector, used only when the IANA bootstrap is unavailable.
	rdapRedirector  = "https://rdap.org/"
	bootstrapMaxAge = 7 * 24 * time.Hour
)

type bootEntry struct {
	pfx netip.Prefix
	url string
}

type bootstrap struct {
	v4 []bootEntry
	v6 []bootEntry
}

type ianaFile struct {
	Publication string         `json:"publication"`
	Services    [][]([]string) `json:"services"`
}

func parseIANA(b []byte) ([]bootEntry, error) {
	var f ianaFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	var out []bootEntry
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
		for _, c := range svc[0] {
			pfx, err := netip.ParsePrefix(c)
			if err != nil {
				continue
			}
			out = append(out, bootEntry{pfx: pfx.Masked(), url: base})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("rdap bootstrap: no usable services")
	}
	// Longest prefix first so a linear scan finds the most specific match.
	sort.SliceStable(out, func(i, j int) bool { return out[i].pfx.Bits() > out[j].pfx.Bits() })
	return out, nil
}

// loadBootstrap reads the cached copy, refreshing it from IANA when missing or
// older than bootstrapMaxAge. A stale cache is preferred over a failed fetch.
func (r *Resolver) loadBootstrap(ctx context.Context) *bootstrap {
	bs := &bootstrap{
		v4: r.bootstrapPart(ctx, bootstrapV4URL, "rdap-bootstrap-v4.json"),
		v6: r.bootstrapPart(ctx, bootstrapV6URL, "rdap-bootstrap-v6.json"),
	}
	return bs
}

func (r *Resolver) bootstrapPart(ctx context.Context, url, name string) []bootEntry {
	path := filepath.Join(r.cacheDir, name)
	if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < bootstrapMaxAge {
		if b, err := os.ReadFile(path); err == nil {
			if e, err := parseIANA(b); err == nil {
				return e
			}
		}
	}
	b, err := r.get(ctx, url, "application/json")
	if err == nil {
		if e, perr := parseIANA(b); perr == nil {
			_ = os.WriteFile(path, b, 0o644)
			return e
		}
	}
	// Fetch failed: fall back to whatever is on disk, however old.
	if b, rerr := os.ReadFile(path); rerr == nil {
		if e, perr := parseIANA(b); perr == nil {
			return e
		}
	}
	return nil
}

// serviceFor returns the RDAP base URL for addr, or the public redirector when
// the bootstrap data is unavailable.
func (r *Resolver) serviceFor(addr netip.Addr) string {
	bs := r.boot.Load()
	if bs != nil {
		list := bs.v4
		if addr.Is6() && !addr.Is4In6() {
			list = bs.v6
		}
		for _, e := range list {
			if e.pfx.Contains(addr.Unmap()) {
				return e.url
			}
		}
	}
	return rdapRedirector
}

// ---------------------------------------------------------------------------
// RDAP response parsing
// ---------------------------------------------------------------------------

type rdapEntity struct {
	Handle     string       `json:"handle"`
	Roles      []string     `json:"roles"`
	VCardArray []any        `json:"vcardArray"`
	Entities   []rdapEntity `json:"entities"`
}

type rdapRemark struct {
	Title       string   `json:"title"`
	Description []string `json:"description"`
}

type rdapIP struct {
	Handle       string       `json:"handle"`
	Name         string       `json:"name"`
	Country      string       `json:"country"`
	StartAddress string       `json:"startAddress"`
	EndAddress   string       `json:"endAddress"`
	Entities     []rdapEntity `json:"entities"`
	Remarks      []rdapRemark `json:"remarks"`
}

// remarksName returns the organisation from the whois "descr" attribute, which
// APNIC and some other registries expose as a remark titled "description"
// instead of a registrant entity. The first line is the organisation; the rest
// is its postal address.
func remarksName(remarks []rdapRemark) string {
	for _, rm := range remarks {
		if !strings.EqualFold(strings.TrimSpace(rm.Title), "description") {
			continue
		}
		for _, line := range rm.Description {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}

// vcardName pulls the "fn" (formatted name) property out of a jCard array.
func vcardName(vcard []any) string {
	if len(vcard) < 2 {
		return ""
	}
	props, ok := vcard[1].([]any)
	if !ok {
		return ""
	}
	for _, p := range props {
		row, ok := p.([]any)
		if !ok || len(row) < 4 {
			continue
		}
		if key, _ := row[0].(string); key != "fn" {
			continue
		}
		if s, ok := row[3].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func hasRole(e rdapEntity, role string) bool {
	for _, r := range e.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// registrantName picks the organisation that owns the netblock. All five RIRs
// expose it as an entity with the "registrant" role, but RIPE also lists the
// maintainer object (handle ...-MNT) under the same role, and the real
// organisation is the one with an ORG- handle.
func registrantName(entities []rdapEntity) string {
	var fallback string
	for _, e := range entities {
		if !hasRole(e, "registrant") {
			continue
		}
		name := vcardName(e.VCardArray)
		if name == "" {
			continue
		}
		if strings.HasSuffix(strings.ToUpper(e.Handle), "-MNT") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(e.Handle), "ORG-") {
			return name
		}
		if fallback == "" {
			fallback = name
		}
	}
	if fallback != "" {
		return fallback
	}
	// Some registries nest the organisation one level down.
	for _, e := range entities {
		if n := registrantName(e.Entities); n != "" {
			return n
		}
	}
	return ""
}

func (r *Resolver) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", r.userAgent)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	return body, nil
}

// fetch queries the responsible RIR for addr and returns the netblock record.
func (r *Resolver) fetch(ctx context.Context, addr netip.Addr) (Record, error) {
	base := r.serviceFor(addr)
	url := base + "ip/" + addr.String()

	r.throttle(ctx)
	body, err := r.get(ctx, url, "application/rdap+json")
	if err != nil {
		return Record{}, err
	}
	var doc rdapIP
	if err := json.Unmarshal(body, &doc); err != nil {
		return Record{}, err
	}

	start, serr := netip.ParseAddr(strings.TrimSpace(doc.StartAddress))
	end, eerr := netip.ParseAddr(strings.TrimSpace(doc.EndAddress))
	if serr != nil || eerr != nil {
		return Record{}, fmt.Errorf("rdap %s: missing range", addr)
	}

	name := registrantName(doc.Entities)
	if name == "" {
		name = remarksName(doc.Remarks)
	}

	rec := Record{
		Name:    name,
		Country: strings.ToUpper(strings.TrimSpace(doc.Country)),
		Netname: strings.TrimSpace(doc.Name),
		Start:   start.Unmap().String(),
		End:     end.Unmap().String(),
		Fetched: time.Now().Unix(),
	}
	if rec.Name == "" {
		rec.Source = sourceEmpty
	} else {
		rec.Source = sourceRDAP
	}
	return rec, nil
}

// throttle serialises outbound RDAP calls and spaces them by minInterval so we
// never trip a registry's per-client rate limit.
func (r *Resolver) throttle(ctx context.Context) {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	if wait := time.Until(r.nextAt); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	r.nextAt = time.Now().Add(r.minInterval)
}

// Command server exposes a self-hosted IP lookup API compatible with the
// response shape of api.ipapi.is.
//
//	GET  /?q=8.8.8.8      single lookup (omit q to look up the caller)
//	POST /                {"ips": ["8.8.8.8", ...]} -> array of results
//	GET  /healthz         readiness probe
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"

	"ipapi/internal/company"
)

const maxBulk = 100

type geoRecord struct {
	CountryCode string  `maxminddb:"country_code"`
	Country     string  `maxminddb:"country"`
	Continent   string  `maxminddb:"continent"`
	State       string  `maxminddb:"state"`
	City        string  `maxminddb:"city"`
	Zip         string  `maxminddb:"zip"`
	Timezone    string  `maxminddb:"timezone"`
	Latitude    float64 `maxminddb:"latitude"`
	Longitude   float64 `maxminddb:"longitude"`
	Accuracy    uint16  `maxminddb:"accuracy"`
}

type asnRecord struct {
	ASN     uint32 `maxminddb:"asn"`
	Org     string `maxminddb:"org"`
	Country string `maxminddb:"country"`
}

// Response mirrors the api.ipapi.is field names. The flags this build cannot
// determine (is_datacenter, is_tor, is_proxy, is_vpn, is_abuser) are omitted
// unless -compat is set, so a missing capability never looks like a negative
// answer.
type Response struct {
	IP      string `json:"ip"`
	IsBogon bool   `json:"is_bogon"`

	IsDatacenter *bool `json:"is_datacenter,omitempty"`
	IsTor        *bool `json:"is_tor,omitempty"`
	IsProxy      *bool `json:"is_proxy,omitempty"`
	IsVPN        *bool `json:"is_vpn,omitempty"`
	IsAbuser     *bool `json:"is_abuser,omitempty"`

	CompanyName    string `json:"company_name"`
	CompanySource  string `json:"company_source,omitempty"`
	CompanyNetwork string `json:"company_network,omitempty"`

	ASNNum       uint32 `json:"asn_num,omitempty"`
	ASNOrg       string `json:"asn_org,omitempty"`
	ASNName      string `json:"asn_name,omitempty"`
	ASNOrgSource string `json:"asn_org_source,omitempty"`

	CC       string  `json:"cc"`
	Country  string  `json:"country,omitempty"`
	State    string  `json:"state,omitempty"`
	City     string  `json:"city,omitempty"`
	Zip      string  `json:"zip,omitempty"`
	Timezone string  `json:"timezone,omitempty"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Accuracy uint16  `json:"accuracy,omitempty"`

	ElapsedMS float64 `json:"elapsed_ms"`
	Error     string  `json:"error,omitempty"`
}

type server struct {
	geo     atomic.Pointer[maxminddb.Reader]
	asn     atomic.Pointer[maxminddb.Reader]
	mmdbDir string
	company *company.Resolver
	compat  bool
	timeout time.Duration
	log     *slog.Logger
}

func main() {
	var (
		addr     = flag.String("addr", ":8080", "listen address")
		mmdbDir  = flag.String("mmdb", "data/mmdb", "directory holding geo.mmdb and asn.mmdb")
		cacheDir = flag.String("cache", "data/cache", "directory for the company (RDAP) cache")
		useRDAP  = flag.Bool("company", true, "resolve company_name and asn_org from whois via RDAP")
		compat   = flag.Bool("compat", false, "emit is_datacenter/is_tor/is_proxy/is_vpn/is_abuser as false for drop-in compatibility")
		timeout  = flag.Duration("timeout", 5*time.Second, "per-request budget for a company lookup")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	s := &server{mmdbDir: *mmdbDir, compat: *compat, timeout: *timeout, log: log}
	if err := s.reload(); err != nil {
		log.Error("cannot open databases (run `make data` first)", "err", err)
		os.Exit(1)
	}
	defer func() {
		if r := s.geo.Load(); r != nil {
			_ = r.Close()
		}
		if r := s.asn.Load(); r != nil {
			_ = r.Close()
		}
	}()

	if *useRDAP {
		res, err := company.New(company.Options{CacheDir: *cacheDir, Logger: log})
		if err != nil {
			log.Error("cannot start company resolver", "err", err)
			os.Exit(1)
		}
		defer res.Close()
		s.company = res
	}

	go s.watchReloads()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		nets, asns := 0, 0
		if s.company != nil {
			nets, asns = s.company.Len(), s.company.ASNLen()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "company_netblocks": nets, "asn_orgs": asns})
	})
	mux.HandleFunc("/", s.handle)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", *addr, "company_lookup", *useRDAP, "compat", *compat)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		if s := <-sig; s == syscall.SIGHUP {
			continue
		}
		break
	}
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, Response{Error: "method not allowed"})
	}
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		q = clientIP(r)
	}
	addr, err := netip.ParseAddr(q)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{IP: q, Error: "invalid IP address"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	writeJSON(w, http.StatusOK, s.lookup(ctx, addr))
}

func (s *server) handlePost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IPs []string `json:"ips"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, Response{Error: "invalid JSON body"})
		return
	}
	if len(body.IPs) == 0 {
		writeJSON(w, http.StatusBadRequest, Response{Error: "field \"ips\" is required"})
		return
	}
	if len(body.IPs) > maxBulk {
		writeJSON(w, http.StatusBadRequest, Response{Error: "at most 100 IPs per request"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout*3)
	defer cancel()

	out := make([]Response, len(body.IPs))
	for i, raw := range body.IPs {
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			out[i] = Response{IP: raw, Error: "invalid IP address"}
			continue
		}
		out[i] = s.lookup(ctx, addr)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) lookup(ctx context.Context, addr netip.Addr) Response {
	start := time.Now()
	addr = addr.Unmap()

	resp := Response{IP: addr.String(), IsBogon: isBogon(addr)}
	if s.compat {
		f := false
		resp.IsDatacenter, resp.IsTor, resp.IsProxy, resp.IsVPN, resp.IsAbuser = &f, &f, &f, &f, &f
	}
	if resp.IsBogon {
		resp.ElapsedMS = msSince(start)
		return resp
	}

	var geo geoRecord
	if res := s.geo.Load().Lookup(addr); res.Found() {
		if err := res.Decode(&geo); err == nil {
			resp.CC = geo.CountryCode
			resp.Country = geo.Country
			resp.State = geo.State
			resp.City = geo.City
			resp.Zip = geo.Zip
			resp.Timezone = geo.Timezone
			resp.Lat = geo.Latitude
			resp.Lon = geo.Longitude
			resp.Accuracy = geo.Accuracy
		}
	}

	var asn asnRecord
	if res := s.asn.Load().Lookup(addr); res.Found() {
		if err := res.Decode(&asn); err == nil {
			resp.ASNNum = asn.ASN
			// RouteViews carries the AS handle (ATT-INTERNET4), not the name of
			// the organisation that registered the AS.
			resp.ASNName = asn.Org
			resp.ASNOrg = asn.Org
			resp.ASNOrgSource = "routeviews"
			if resp.CC == "" {
				resp.CC = asn.Country
			}
		}
	}

	if s.company != nil && resp.ASNNum != 0 {
		if rec, ok := s.company.LookupASN(ctx, resp.ASNNum); ok {
			resp.ASNOrg = rec.Name
			resp.ASNOrgSource = "whois_rdap"
			if rec.Netname != "" {
				resp.ASNName = rec.Netname
			}
		}
	}

	// company_name comes from netblock whois. Fall back to the ASN name so the
	// field is never empty, but say which source was used.
	if s.company != nil {
		if rec, ok := s.company.Lookup(ctx, addr); ok {
			resp.CompanyName = rec.Name
			resp.CompanySource = "whois_rdap"
			if rec.Start != "" {
				resp.CompanyNetwork = rec.Start + " - " + rec.End
			}
		}
	}
	if resp.CompanyName == "" && resp.ASNOrg != "" {
		resp.CompanyName = resp.ASNOrg
		resp.CompanySource = "asn_org"
	}

	resp.ElapsedMS = msSince(start)
	return resp
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// clientIP resolves the caller's address, honouring a reverse proxy header.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

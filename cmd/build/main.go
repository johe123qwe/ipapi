// Command build converts the ipapi.is geolocation CSVs and the iptoasn TSV
// into MMDB databases that the server memory-maps at runtime.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func main() {
	src := flag.String("src", "data/src", "directory holding the downloaded source files")
	out := flag.String("out", "data/mmdb", "directory to write the .mmdb files to")
	only := flag.String("only", "", "build only one database: geo or asn")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if *only == "" || *only == "geo" {
		if err := buildGeo(*src, *out); err != nil {
			log.Fatalf("geo: %v", err)
		}
	}
	if *only == "" || *only == "asn" {
		if err := buildASN(*src, *out); err != nil {
			log.Fatalf("asn: %v", err)
		}
	}
}

func newTree(dbType string) (*mmdbwriter.Tree, error) {
	return mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: dbType,
		RecordSize:   28,
		IPVersion:    6,
		// The geolocation data covers a few ranges the writer treats as
		// reserved; keep them rather than failing the build.
		IncludeReservedNetworks: true,
		Description:             map[string]string{"en": "Built from public data by cmd/build"},
	})
}

func writeTree(t *mmdbwriter.Tree, path string, n int) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, err := t.WriteTo(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	log.Printf("wrote %s (%d networks, %.1f MiB)", path, n, float64(written)/(1<<20))
	return nil
}

// ---------------------------------------------------------------------------
// Geolocation
// ---------------------------------------------------------------------------

// Column order of the ipapi.is geolocation CSVs.
const (
	gIPVersion = iota
	gStartIP
	gEndIP
	gContinent
	gCountryCode
	gCountry
	gState
	gCity
	gZip
	gTimezone
	gLatitude
	gLongitude
	gAccuracy
	gSource
	gFieldCount
)

func buildGeo(src, out string) error {
	tree, err := newTree("ipapi-geo")
	if err != nil {
		return err
	}
	total := 0
	for _, name := range []string{"geolocationDatabaseIPv4.csv", "geolocationDatabaseIPv6.csv"} {
		path := filepath.Join(src, name)
		n, err := loadGeoCSV(tree, path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		total += n
	}
	return writeTree(tree, filepath.Join(out, "geo.mmdb"), total)
}

func loadGeoCSV(tree *mmdbwriter.Tree, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.ReuseRecord = true
	r.FieldsPerRecord = gFieldCount

	if _, err := r.Read(); err != nil { // header
		return 0, err
	}

	n, skipped := 0, 0
	start := time.Now()
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A malformed line should not abort a multi-million row import.
			skipped++
			continue
		}
		startIP := net.ParseIP(rec[gStartIP])
		endIP := net.ParseIP(rec[gEndIP])
		if startIP == nil || endIP == nil {
			skipped++
			continue
		}
		data := mmdbtype.Map{
			"country_code": mmdbtype.String(rec[gCountryCode]),
			"country":      mmdbtype.String(rec[gCountry]),
			"continent":    mmdbtype.String(rec[gContinent]),
		}
		putStr(data, "state", rec[gState])
		putStr(data, "city", rec[gCity])
		putStr(data, "zip", rec[gZip])
		putStr(data, "timezone", rec[gTimezone])
		putFloat(data, "latitude", rec[gLatitude])
		putFloat(data, "longitude", rec[gLongitude])
		if acc, err := strconv.Atoi(rec[gAccuracy]); err == nil {
			data["accuracy"] = mmdbtype.Uint16(acc)
		}

		if err := tree.InsertRange(startIP, endIP, data); err != nil {
			skipped++
			continue
		}
		n++
		if n%500_000 == 0 {
			log.Printf("%s: %d networks (%s)", filepath.Base(path), n, time.Since(start).Round(time.Second))
		}
	}
	log.Printf("%s: %d networks loaded, %d skipped (%s)", filepath.Base(path), n, skipped, time.Since(start).Round(time.Second))
	return n, nil
}

func putStr(m mmdbtype.Map, key, v string) {
	if v = strings.TrimSpace(v); v != "" {
		m[mmdbtype.String(key)] = mmdbtype.String(v)
	}
}

func putFloat(m mmdbtype.Map, key, v string) {
	if v = strings.TrimSpace(v); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m[mmdbtype.String(key)] = mmdbtype.Float64(f)
		}
	}
}

// ---------------------------------------------------------------------------
// IP to ASN (iptoasn.com combined TSV)
// ---------------------------------------------------------------------------

func buildASN(src, out string) error {
	path := filepath.Join(src, "ip2asn-combined.tsv")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	tree, err := newTree("ipapi-asn")
	if err != nil {
		return err
	}

	r := csv.NewReader(f)
	r.Comma = '\t'
	r.ReuseRecord = true
	r.FieldsPerRecord = 5
	r.LazyQuotes = true

	n, skipped := 0, 0
	start := time.Now()
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped++
			continue
		}
		asn, err := strconv.ParseUint(rec[2], 10, 32)
		if err != nil || asn == 0 {
			// AS number 0 marks an unannounced range.
			skipped++
			continue
		}
		startIP, endIP := net.ParseIP(rec[0]), net.ParseIP(rec[1])
		if startIP == nil || endIP == nil {
			skipped++
			continue
		}
		data := mmdbtype.Map{"asn": mmdbtype.Uint32(asn)}
		putStr(data, "org", rec[4])
		if cc := strings.ToUpper(strings.TrimSpace(rec[3])); cc != "" && cc != "NONE" {
			data["country"] = mmdbtype.String(cc)
		}
		if err := tree.InsertRange(startIP, endIP, data); err != nil {
			skipped++
			continue
		}
		n++
	}
	log.Printf("ip2asn: %d networks loaded, %d skipped (%s)", n, skipped, time.Since(start).Round(time.Second))
	return writeTree(tree, filepath.Join(out, "asn.mmdb"), n)
}

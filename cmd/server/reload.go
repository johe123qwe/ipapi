package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

// readerGrace is how long the previous MMDB readers are kept alive after a
// reload. Lookups hold a pointer for the duration of one request only, so any
// value comfortably above the request timeout avoids unmapping memory that an
// in-flight request is still reading.
const readerGrace = 60 * time.Second

// reload opens the databases from disk and atomically swaps them in. It is
// used both at startup and on SIGHUP, so refreshing the data never needs a
// restart: `make data` writes new files, then SIGHUP picks them up.
func (s *server) reload() error {
	geo, err := maxminddb.Open(filepath.Join(s.mmdbDir, "geo.mmdb"))
	if err != nil {
		return err
	}
	asn, err := maxminddb.Open(filepath.Join(s.mmdbDir, "asn.mmdb"))
	if err != nil {
		_ = geo.Close()
		return err
	}

	oldGeo := s.geo.Swap(geo)
	oldASN := s.asn.Swap(asn)

	if oldGeo != nil || oldASN != nil {
		time.AfterFunc(readerGrace, func() {
			if oldGeo != nil {
				_ = oldGeo.Close()
			}
			if oldASN != nil {
				_ = oldASN.Close()
			}
		})
	}
	return nil
}

// watchReloads reloads the databases whenever SIGHUP arrives. A failed reload
// is logged and the previous databases stay in use.
func (s *server) watchReloads() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	for range ch {
		if err := s.reload(); err != nil {
			s.log.Error("reload failed, keeping current databases", "err", err)
			continue
		}
		s.log.Info("databases reloaded")
	}
}

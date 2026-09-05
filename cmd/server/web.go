package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// indexHTML is the whole browser UI: one self-contained file with no external
// requests, so the page also works on a host that cannot reach the Internet.
//
//go:embed web/index.html
var indexHTML []byte

var (
	indexETag = func() string {
		sum := sha256.Sum256(indexHTML)
		return `"` + hex.EncodeToString(sum[:8]) + `"`
	}()
	indexModTime = time.Now()
)

// wantsHTML reports whether the caller is a browser asking for the UI rather
// than a client asking for the API. The Accept header is scanned in the order
// the client sent it and the first decisive media type wins: browsers lead with
// text/html, while curl and fetch() send */* and keep getting JSON. An explicit
// ?format=json always wins, so the API stays reachable from a browser too.
func wantsHTML(r *http.Request) bool {
	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "json":
		return false
	case "html":
		return true
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "text/html", "application/xhtml+xml":
			return true
		case "application/json", "text/plain", "text/*":
			return false
		}
	}
	return false
}

// serveIndex writes the UI. It is revalidated rather than cached outright, so a
// redeployed binary never leaves a stale page behind.
func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", indexETag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "index.html", indexModTime, bytes.NewReader(indexHTML))
}

package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type webUIAsset struct {
	once sync.Once
	body []byte
	gzip []byte
	etag string
}

// Keep immutable, compressed UI bytes in memory after first access. Never cache API data.
func newWebUIHandler(root fs.FS) http.Handler {
	var assets sync.Map
	serve := func(w http.ResponseWriter, r *http.Request, name string) bool {
		// Check before storing: arbitrary missing URLs cannot grow the asset cache.
		info, err := fs.Stat(root, name)
		if err != nil || info.IsDir() {
			return false
		}
		entry, _ := assets.LoadOrStore(name, &webUIAsset{})
		asset := entry.(*webUIAsset)
		asset.once.Do(func() {
			asset.body, _ = fs.ReadFile(root, name)
			asset.etag = fmt.Sprintf(`W/"%x"`, sha256.Sum256(asset.body))
			ext := path.Ext(name)
			if len(asset.body) >= 1024 && (ext == ".js" || ext == ".css" || ext == ".html" || ext == ".txt" || ext == ".json" || ext == ".svg") {
				var compressed bytes.Buffer
				writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
				_, _ = writer.Write(asset.body)
				_ = writer.Close()
				if compressed.Len() < len(asset.body) {
					asset.gzip = compressed.Bytes()
				}
			}
		})
		if asset.body == nil {
			http.Error(w, "UI asset unavailable", http.StatusInternalServerError)
			return true
		}
		w.Header().Set("ETag", asset.etag)
		if strings.HasPrefix(name, "_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// HTML, route payloads and unversioned files must survive a new deployment.
			w.Header().Set("Cache-Control", "no-cache")
		}
		if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Header().Set("Vary", "Accept-Encoding")
		body := asset.body
		if asset.gzip != nil {
			if encodingQuality(r.Header.Get("Accept-Encoding"), "gzip", 0) > 0 {
				body = asset.gzip
				w.Header().Set("Content-Encoding", "gzip")
			}
		}
		if w.Header().Get("Content-Encoding") == "" && encodingQuality(r.Header.Get("Accept-Encoding"), "identity", 1) == 0 {
			w.Header().Del("Content-Type")
			w.Header().Del("ETag")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "Accept-Encoding")
			http.Error(w, "No acceptable content encoding", http.StatusNotAcceptable)
			return true
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
		return true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		for _, candidate := range []string{p, p + "/index.html", p + ".html"} {
			if serve(w, r, candidate) {
				return
			}
		}
		// Next's Windows static exporter can preserve segment subdirectories while
		// the browser requests flattened, dot-separated segment filenames.
		base := path.Base(p)
		if strings.HasPrefix(base, "__next.") && strings.HasSuffix(base, ".txt") {
			segments := strings.TrimSuffix(strings.TrimPrefix(base, "__next."), ".txt")
			nested := path.Join(path.Dir(p), "__next."+strings.ReplaceAll(segments, ".", "/")+".txt")
			if nested != p && serve(w, r, nested) {
				return
			}
		}
		// Missing chunks must be 404, not successful HTML that the router tries to parse.
		if strings.HasPrefix(p, "_next/") || path.Ext(p) != "" {
			http.NotFound(w, r)
			return
		}
		if !serve(w, r, "index.html") {
			http.NotFound(w, r)
		}
	})
}

// Explicit encoding preferences override the wildcard. Identity is acceptable by
// default, except when identity or an otherwise unmatched wildcard is rejected.
func encodingQuality(value, encoding string, fallback float64) float64 {
	wildcard := -1.0
	for _, entry := range strings.Split(value, ",") {
		parts := strings.Split(entry, ";")
		name := strings.TrimSpace(parts[0])
		if !strings.EqualFold(name, encoding) && name != "*" {
			continue
		}
		quality := 1.0
		for _, param := range parts[1:] {
			key, raw, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(key, "q") {
				var err error
				quality, err = strconv.ParseFloat(strings.TrimSpace(raw), 64)
				if err != nil || !(quality >= 0 && quality <= 1) {
					quality = 0
				}
			}
		}
		if strings.EqualFold(name, encoding) {
			return quality
		}
		wildcard = quality
	}
	if wildcard >= 0 {
		if encoding != "identity" || wildcard == 0 {
			return wildcard
		}
	}
	return fallback
}

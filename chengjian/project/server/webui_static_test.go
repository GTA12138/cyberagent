package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebUIStaticNavigationAssets(t *testing.T) {
	script := strings.Repeat("const evidence = 'real';\n", 200)
	root := fstest.MapFS{
		"_next/static/font.woff2":                                     {Data: []byte("font")},
		"function/tasks/__next.!KG1haW4p/function/tasks/__PAGE__.txt": {Data: []byte("segment-payload")},
		"index.html":                    {Data: []byte("<html>shell</html>")},
		"function/tasks/index.html":     {Data: []byte("<html>tasks</html>")},
		"function/tasks/index.txt":      {Data: []byte("route-payload")},
		"_next/static/chunks/hashed.js": {Data: []byte(script)},
	}
	handler := newWebUIHandler(root)
	request := func(url, encoding, etag, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Accept-Encoding", encoding)
		req.Header.Set("If-None-Match", etag)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	compressed := request("/_next/static/chunks/hashed.js", "gzip", "", "GET")
	if compressed.Code != http.StatusOK || compressed.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected compressed JS: %d %v", compressed.Code, compressed.Header())
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(body) != script {
		t.Fatal("compressed bytes changed the JS")
	}
	if got := compressed.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatal(got)
	}
	if got := compressed.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatal(got)
	}
	plain := request("/_next/static/chunks/hashed.js", "gzip;q=0, identity", "", "GET")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != script {
		t.Fatal("gzip q=0 not respected")
	}
	for _, encoding := range []string{"gzip;q=0, identity;q=0", "*;q=0"} {
		if rejected := request("/_next/static/chunks/hashed.js", encoding, "", "GET"); rejected.Code != http.StatusNotAcceptable {
			t.Fatalf("rejected encodings should return 406: %s", encoding)
		}
	}
	if font := request("/_next/static/font.woff2", "", "", "GET"); font.Code != http.StatusOK || font.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("uncompressed asset must vary because encoding rejection changes its status")
	}
	if rejected := request("/_next/static/font.woff2", "identity;q=0", "", "GET"); rejected.Code != http.StatusNotAcceptable || rejected.Header().Get("Cache-Control") != "no-store" || rejected.Header().Get("ETag") != "" || rejected.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("encoding rejection must never become an immutable cached error")
	}
	if accepted := request("/_next/static/chunks/hashed.js", "*;q=0, gzip;q=1", "", "GET"); accepted.Code != http.StatusOK || accepted.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("explicit gzip preference must override wildcard rejection")
	}
	if cached := request("/_next/static/chunks/hashed.js", "gzip", compressed.Header().Get("ETag"), "GET"); cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatal("conditional asset request did not produce 304")
	}
	for _, url := range []string{"/function/tasks/", "/function/tasks/index.txt"} {
		response := request(url, "", "", "GET")
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("route must revalidate: %s %d", url, response.Code)
		}
	}
	if head := request("/_next/static/chunks/hashed.js", "gzip", "", "HEAD"); head.Body.Len() != 0 || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("HEAD not preserved")
	}
	for _, url := range []string{"/_next/static/missing.js", "/missing.css", "/function/unknown/index.txt"} {
		if response := request(url, "", "", "GET"); response.Code != http.StatusNotFound {
			t.Fatalf("missing asset returned HTML: %s", url)
		}
	}
	segment := request("/function/tasks/__next.!KG1haW4p.function.tasks.__PAGE__.txt", "", "", "GET")
	if segment.Code != http.StatusOK || segment.Body.String() != "segment-payload" || segment.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("Windows static-export segment must resolve without returning HTML")
	}
	if missing := request("/function/tasks/__next.!KG1haW4p.missing.txt", "", "", "GET"); missing.Code != http.StatusNotFound {
		t.Fatal("nonexistent export segment must remain 404")
	}
	if response := request("/unknown-route", "", "", "GET"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "shell") {
		t.Fatal("SPA route fallback not preserved")
	}
}

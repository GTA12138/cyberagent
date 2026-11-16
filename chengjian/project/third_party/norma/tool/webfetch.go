package tool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/norma/permission"
	"golang.org/x/net/html"
)

// WebFetchConfig tunes the network behavior of a WebFetch tool.
type WebFetchConfig struct {
	// Proxy, when set, routes requests through this http(s) proxy URL. Empty = direct.
	Proxy string
	// CACert, when set, is a path to a PEM CA certificate added to the system
	// trust pool. This lets WebFetch *verify* HTTPS fetched through a MITM/recording
	// proxy that re-signs certificates with its own CA — the proper alternative to
	// InsecureTLS, keeping verification on for every other connection. Preferred
	// over InsecureTLS; ignored when InsecureTLS is set.
	CACert string
	// InsecureTLS skips TLS certificate verification entirely (equivalent to
	// curl -k). A blunt fallback when no CA cert is available; prefer CACert.
	InsecureTLS bool
}

// NewWebFetch builds the WebFetch tool: fetches a URL over HTTP(S) and returns
// its content as Markdown (HTML is parsed and rendered; non-HTML is returned
// as-is). With extract=true it also appends a RECON section that preserves the
// signal HTML→Markdown normally discards — comments, <script> sources, inline
// endpoints, forms/inputs (including hidden) and meta tags — useful for security
// reconnaissance. cfg optionally routes requests through a proxy and/or skips
// TLS verification (see WebFetchConfig). It is network-facing, so it is not part
// of DefaultTools and requires permission to run.
func NewWebFetch(cfg WebFetchConfig) CoreTool {
	return Build(Spec{
		Name:        "WebFetch",
		Description: "Fetches an HTTP(S) page or API response. Default HTML output is Markdown; extract=true includes hidden forms/comments/script references. Set raw=true to inspect unmodified HTML or text assets (JS/CSS/source maps). Original received bytes are saved to body_file for Read byte paging. Large responses explicitly report incomplete reads. Configured proxies are never silently bypassed. This makes an external network request.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":     map[string]any{"type": "string", "description": "The absolute http(s) URL. For JS/CSS/source maps also set raw=true."},
				"extract": map[string]any{"type": "boolean", "description": "When true, append a RECON section preserving HTML comments, <script src>, inline-script endpoints, forms/inputs (including hidden), links and meta tags. Default false."},
				"raw":     map[string]any{"type": "boolean", "description": "Return raw text without HTML conversion; required for JS/CSS/source maps. Original bytes are also saved for lossless paging."},
			},
			"required": []any{"url"},
		},
		// Network access is open-world: require approval rather than auto-allow.
		Permissions: func(_ context.Context, input json.RawMessage, _ permission.Context) permission.Decision {
			var in struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(input, &in)
			return permission.AskUser("fetch external URL: " + in.URL)
		},
		Run: webFetchRunner(cfg),
	})
}

const maxFetchBytes = 2 << 20 // 2 MiB

// newFetchClient returns an HTTP client honoring the proxy / TLS config.
func newFetchClient(cfg WebFetchConfig) *http.Client {
	if cfg.Proxy == "" && !cfg.InsecureTLS && cfg.CACert == "" {
		return &http.Client{}
	}
	tr := &http.Transport{}
	if cfg.Proxy != "" {
		if pu, err := url.Parse(cfg.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	switch {
	case cfg.InsecureTLS:
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	case cfg.CACert != "":
		// Trust the proxy's CA in addition to the system roots, so certs it
		// re-signs verify while every other connection stays fully checked.
		if pem, err := os.ReadFile(cfg.CACert); err == nil {
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			if pool.AppendCertsFromPEM(pem) {
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
		}
	}
	return &http.Client{Transport: tr}
}

// proxyLikelyErr reports whether a fetch error is the kind a recording/MITM proxy
// introduces — a closed tunnel (EOF / connection reset), a failed CONNECT, or a
// TLS/HTTP2 mismatch — so a direct (no-proxy) retry may succeed. Name-resolution
// failures and caller cancellation/timeout are target- or caller-side problems a
// direct retry won't fix, so those return false (no retry).
func proxyLikelyErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, n := range []string{"no such host", "context canceled", "context deadline", "timeout"} {
		if strings.Contains(s, n) {
			return false
		}
	}
	for _, p := range []string{
		"eof", "connection reset", "connection refused", "proxyconnect",
		"broken pipe", "forcibly closed", "malformed", "http2", "http/2",
		"tls:", "protocol error",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// staticAssetExts are file extensions WebFetch refuses: linked resources (scripts,
// styles, source maps, images, fonts, media, wasm) that are not page content.
var staticAssetExts = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".css": true, ".map": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".avif": true, ".ico": true, ".bmp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".wasm": true, ".mp4": true, ".webm": true, ".mp3": true, ".wav": true, ".ogg": true,
}

// staticAssetExt returns the offending extension (with dot) if rawURL points at a
// static asset, or "" otherwise. The query string and fragment are ignored, so
// "/app.js?v=3" is still caught.
func staticAssetExt(rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		path = u.Path
	}
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:] // last segment only
	}
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return ""
	}
	if ext := strings.ToLower(path[dot:]); staticAssetExts[ext] {
		return ext
	}
	return ""
}

func webFetchRunner(cfg WebFetchConfig) func(context.Context, json.RawMessage, *ToolContext) (Result, error) {
	client := newFetchClient(cfg)
	return func(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
		var in struct {
			URL     string `json:"url"`
			Extract bool   `json:"extract"`
			Raw     bool   `json:"raw"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return Result{}, err
		}
		if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
			return Errorf("Error: url must start with http:// or https://"), nil
		}
		if ext := staticAssetExt(in.URL); ext != "" {
			textAsset := ext == ".js" || ext == ".mjs" || ext == ".cjs" || ext == ".css" || ext == ".map"
			if !in.Raw || !textAsset {
				return Errorf("Error: static asset (" + ext + "). For JS/CSS/source maps set raw=true; binary assets require a binary-capable HTTP tool."), nil
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, in.URL, nil)
		if err != nil {
			return Errorf("Error: " + err.Error()), nil
		}
		req.Header.Set("user-agent", "norma/0.4")
		resp, err := client.Do(req)
		if err != nil {
			return Errorf("Error fetching URL: " + err.Error()), nil
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
		if readErr != nil {
			return Errorf("response_read_failed: incomplete=true; " + readErr.Error()), nil
		}
		complete := len(body) <= maxFetchBytes
		if !complete {
			body = body[:maxFetchBytes]
		}
		rawPath, saveErr := persistFetchBytes(tc, body)
		if saveErr != nil {
			return Errorf("response_preservation_failed: " + saveErr.Error()), nil
		}

		header := fmt.Sprintf("[%d %s] %s\n", resp.StatusCode, http.StatusText(resp.StatusCode), in.URL)
		header += fmt.Sprintf("[body_file=%s received_bytes=%d read_complete=%t content_length=%d; Read byte_offset=0/next_byte_offset for raw bytes]\n", rawPath, len(body), complete, resp.ContentLength)
		if !complete {
			header += fmt.Sprintf("[response_limit_reached: incomplete=true next_range_offset=%d; use an HTTP Range request and verify 206/Content-Range/ETag before combining chunks]\n", len(body))
		}
		// Cheap fingerprint signal that HTML→Markdown would otherwise hide.
		if sv := resp.Header.Get("Server"); sv != "" {
			header += "Server: " + sv + "\n"
		}
		if xp := resp.Header.Get("X-Powered-By"); xp != "" {
			header += "X-Powered-By: " + xp + "\n"
		}

		var out string
		if !in.Raw && strings.Contains(strings.ToLower(resp.Header.Get("content-type")), "html") {
			doc, perr := html.Parse(strings.NewReader(string(body)))
			if perr != nil { // html.Parse is lenient; this is effectively unreachable
				out = string(body)
			} else {
				out = htmlToMarkdown(doc)
				if in.Extract {
					if recon := extractRecon(doc); recon != "" {
						out += "\n\n" + recon
					}
				}
			}
		} else {
			out = string(body)
		}
		return Text(Capture(tc, header+strings.TrimSpace(out))), nil
	}
}

func persistFetchBytes(tc *ToolContext, body []byte) (string, error) {
	dir := filepath.Join(os.TempDir(), "chengjian-tool-output")
	if tc != nil {
		if tc.OutputDir != "" {
			dir = tc.OutputDir
		} else if tc.WorkingDir != "" {
			dir = filepath.Join(tc.WorkingDir, "cmd-output")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "fetch-*.body")
	if err != nil {
		return "", err
	}
	_, err = f.Write(body)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return f.Name(), nil
}

var (
	reWS         = regexp.MustCompile(`[ \t]+`)
	reLineTrim   = regexp.MustCompile(` *\n *`) // strip spaces hugging a newline
	reBlankLines = regexp.MustCompile(`\n{2,}`) // collapse blank lines → one newline
	// reEndpoint matches quoted absolute URLs and root-relative paths in inline JS.
	reEndpoint = regexp.MustCompile("[\"'`](https?://[^\"'`\\s]+|/[A-Za-z0-9._~$&'()*+,;=:@%/?#\\[\\]-]{2,})[\"'`]")
)

// skipMarkdown are elements whose subtree carries no readable prose.
var skipMarkdown = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true}

// htmlToMarkdown renders the visible content of a parsed document as Markdown.
// Text nodes are already entity-decoded by the parser, so no unescaping is needed.
func htmlToMarkdown(doc *html.Node) string {
	var b strings.Builder
	renderNode(&b, doc)
	s := reWS.ReplaceAllString(b.String(), " ")
	s = reLineTrim.ReplaceAllString(s, "\n")   // drop indentation / trailing spaces per line
	s = reBlankLines.ReplaceAllString(s, "\n") // no blank lines — compact, token-lean output
	return strings.TrimSpace(s)
}

func renderNode(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(strings.ReplaceAll(n.Data, "\n", " "))
		return
	case html.ElementNode:
		if skipMarkdown[n.Data] {
			return
		}
	default:
		if n.Type != html.DocumentNode {
			return
		}
	}
	tag := ""
	if n.Type == html.ElementNode {
		tag = n.Data
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		b.WriteString("\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " ")
		renderChildren(b, n)
		b.WriteString("\n\n")
	case "title":
		b.WriteString("\n\n# ")
		renderChildren(b, n)
		b.WriteString("\n\n")
	case "p", "div", "section", "article", "header", "footer", "main", "ul", "ol", "table", "tr", "blockquote":
		b.WriteString("\n\n")
		renderChildren(b, n)
		b.WriteString("\n\n")
	case "br":
		b.WriteString("\n")
	case "hr":
		b.WriteString("\n\n---\n\n")
	case "li":
		b.WriteString("\n- ")
		renderChildren(b, n)
	case "a":
		var inner strings.Builder
		renderChildren(&inner, n)
		text := strings.TrimSpace(inner.String())
		if href := attr(n, "href"); href != "" && text != "" {
			fmt.Fprintf(b, "[%s](%s)", text, href)
		} else {
			b.WriteString(text)
		}
	case "img":
		if src := attr(n, "src"); src != "" {
			fmt.Fprintf(b, "![%s](%s)", attr(n, "alt"), src)
		}
	case "strong", "b":
		b.WriteString("**")
		renderChildren(b, n)
		b.WriteString("**")
	case "em", "i":
		b.WriteString("*")
		renderChildren(b, n)
		b.WriteString("*")
	case "code":
		b.WriteString("`")
		renderChildren(b, n)
		b.WriteString("`")
	case "pre":
		var inner strings.Builder
		textOnly(&inner, n)
		b.WriteString("\n\n```\n" + strings.TrimSpace(inner.String()) + "\n```\n\n")
	default:
		renderChildren(b, n)
	}
}

func renderChildren(b *strings.Builder, n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderNode(b, c)
	}
}

// extractRecon walks the document for security-relevant signal that Markdown
// rendering drops: comments, script sources, inline endpoints, forms, meta, links.
func extractRecon(doc *html.Node) string {
	var comments, scriptSrcs, inlineEndpoints, forms, metas, links []string
	seenLink := map[string]bool{}

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.CommentNode:
			if c := oneLine(n.Data, 300); c != "" {
				comments = append(comments, c)
			}
		case html.ElementNode:
			switch n.Data {
			case "script":
				if src := attr(n, "src"); src != "" {
					scriptSrcs = append(scriptSrcs, src)
				} else {
					var body strings.Builder
					textOnly(&body, n)
					for _, m := range reEndpoint.FindAllStringSubmatch(body.String(), 40) {
						inlineEndpoints = append(inlineEndpoints, m[1])
					}
				}
			case "form":
				forms = append(forms, describeForm(n))
			case "meta":
				name := attr(n, "name")
				if name == "" {
					name = attr(n, "property")
				}
				if content := attr(n, "content"); name != "" && content != "" {
					metas = append(metas, name+"="+oneLine(content, 120))
				}
			case "a", "link":
				if h := attr(n, "href"); h != "" && !seenLink[h] {
					seenLink[h] = true
					links = append(links, h)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var b strings.Builder
	b.WriteString("=== RECON ===")
	reconSection(&b, "COMMENTS", comments)
	reconSection(&b, "SCRIPTS (src)", dedup(scriptSrcs))
	reconSection(&b, "INLINE ENDPOINTS", dedup(inlineEndpoints))
	reconSection(&b, "FORMS", forms)
	reconSection(&b, "META", metas)
	reconSection(&b, "LINKS", capSlice(links, 100))
	if b.Len() == len("=== RECON ===") {
		return "" // nothing worth reporting
	}
	return b.String()
}

// describeForm summarizes a <form> as "METHOD action → name(type[=value]) …".
// Hidden inputs keep their value (CSRF tokens, parameter defaults matter in recon).
func describeForm(form *html.Node) string {
	method := strings.ToUpper(attr(form, "method"))
	if method == "" {
		method = "GET"
	}
	var inputs []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "input", "textarea", "select":
				typ := attr(n, "type")
				if typ == "" {
					typ = n.Data
				}
				desc := attr(n, "name") + "(" + typ
				if typ == "hidden" {
					if v := attr(n, "value"); v != "" {
						desc += "=" + oneLine(v, 60)
					}
				}
				inputs = append(inputs, desc+")")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return fmt.Sprintf("%s %s → %s", method, attr(form, "action"), strings.Join(inputs, " "))
}

// --- small helpers ---

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textOnly(b *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		textOnly(b, c)
	}
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func reconSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n\n## " + title + "\n")
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func capSlice(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return append(in[:n:n], fmt.Sprintf("… (%d more)", len(in)-n))
}

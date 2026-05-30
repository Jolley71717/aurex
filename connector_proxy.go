package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// proxyConnectorUI forwards /connector/{id}/* to a connector's browsable web
// UI (Connector.WebURL()). It's the generic sibling of the bespoke Gas City
// proxy in gcproxy.go — Gas City needs hand-tuned HTML rewriting, but every
// other connector with a web_url gets embedded through here so the in-app
// iframe launcher works.
//
// Why a proxy instead of pointing the iframe straight at the upstream origin:
//   - aurex is served over HTTPS on the tailnet, so an iframe to a plain
//     http://localhost:3100 origin is blocked as mixed content. Going through
//     aurex keeps the iframe same-origin and same-scheme.
//   - Upstreams commonly send X-Frame-Options / CSP frame-ancestors that would
//     refuse embedding. We strip those on the way back (see modifyResponse).
//
// Path/asset handling is best-effort: we inject a <base> tag and rewrite
// root-absolute asset references so an SPA's bundles load back through the
// proxy. The client always offers an "open in new tab" escape hatch (which
// targets this same proxy path) for apps whose asset scheme we can't rewrite.
func (m *ConnectorsManager) proxyConnectorUI(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c := m.Get(id)
	if c == nil {
		http.Error(w, "connector not found", http.StatusNotFound)
		return
	}
	if !c.Enabled {
		http.Error(w, "connector is disabled", http.StatusServiceUnavailable)
		return
	}
	web := c.WebURL()
	if web == "" {
		http.Error(w, "connector has no web UI", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(web)
	if err != nil {
		http.Error(w, "connector web_url invalid: "+err.Error(), http.StatusInternalServerError)
		return
	}

	prefix := "/connector/" + id
	token := readTokenFile(c.TokenRef)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = connectorModifyResponse(prefix)

	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		req.Host = target.Host
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	r2 := r.Clone(r.Context())
	r2.URL.Path = stripPrefix(r.URL.Path, prefix)
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	proxy.ServeHTTP(w, r2)
}

// connectorModifyResponse strips frame-blocking headers, keeps redirects under
// the proxy prefix, and rewrites HTML so assets resolve back through aurex.
func connectorModifyResponse(prefix string) func(*http.Response) error {
	return func(resp *http.Response) error {
		// Allow embedding: drop the headers that would refuse it.
		resp.Header.Del("X-Frame-Options")
		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			if stripped := stripCSPFrameAncestors(csp); stripped == "" {
				resp.Header.Del("Content-Security-Policy")
			} else {
				resp.Header.Set("Content-Security-Policy", stripped)
			}
		}

		// Keep upstream redirects inside the proxy mount so the iframe doesn't
		// navigate out to a bare (mixed-content) origin.
		if loc := resp.Header.Get("Location"); loc != "" {
			if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, prefix+"/") {
				resp.Header.Set("Location", prefix+loc)
			}
		}

		// Only HTML needs body rewriting. HEAD has no body.
		if resp.Request != nil && resp.Request.Method == http.MethodHead {
			return nil
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), "text/html") {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read connector body: %w", err)
		}
		_ = resp.Body.Close()
		out := rewriteConnectorHTML(body, prefix)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
		// A rewritten body invalidates any content hash the upstream set.
		resp.Header.Del("ETag")
		resp.Header.Del("Content-MD5")
		return nil
	}
}

// rewriteConnectorHTML injects a <base> tag (so relative URLs resolve through
// the proxy) and rewrites root-absolute src/href/action references to the
// proxy prefix (so bundlers that emit "/assets/..." still load).
func rewriteConnectorHTML(body []byte, prefix string) []byte {
	// Rewrite root-absolute asset refs FIRST, then inject <head> shims —
	// otherwise the rewrite pass would clobber the injected tags' own hrefs.
	s := rewriteRootAbsolute(string(body), prefix)

	// Inject (in order) a runtime URL-rebasing shim then a <base> tag, as the
	// first children of <head> so they run before the app's own bundles.
	//   - <base> handles document-relative URLs and HTML resources.
	//   - the shim handles RUNTIME root-absolute calls (fetch/XHR/EventSource)
	//     that <base> can't touch — without it an SPA's `fetch('/health')`
	//     escapes the proxy prefix and 404s against aurex's root.
	inject := connectorRuntimeShim(prefix)
	if !strings.Contains(strings.ToLower(s), "<base ") {
		inject += `<base href="` + prefix + `/">`
	}
	lower := strings.ToLower(s)
	if i := strings.Index(lower, "<head"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			pos := i + j + 1
			s = s[:pos] + inject + s[pos:]
		}
	}

	return []byte(s)
}

// connectorRuntimeShim returns a <script> that monkey-patches fetch, XHR, and
// EventSource so root-absolute URLs ("/api/x", "/health") are rewritten onto
// the proxy prefix. Protocol-relative ("//cdn"), already-prefixed, and
// document-relative URLs are left alone. This is what makes a prefix-unaware
// SPA (e.g. Paperclip) work inside the /connector/{id} iframe.
func connectorRuntimeShim(prefix string) string {
	// prefix has no trailing slash here (e.g. /connector/paperclip-default).
	p, _ := json.Marshal(prefix)
	return `<script>(function(){var P=` + string(p) + `;` +
		`function fix(u){return (typeof u==="string"&&u.charAt(0)==="/"&&u.charAt(1)!=="/"&&u.lastIndexOf(P+"/",0)!==0)?P+u:u;}` +
		`var of=window.fetch;if(of){window.fetch=function(i,init){try{if(typeof i==="string"){i=fix(i);}else if(i&&i.url){i=new Request(fix(i.url),i);}}catch(e){}return of.call(this,i,init);};}` +
		`var xo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){try{u=fix(u);}catch(e){}return xo.apply(this,[m,u].concat([].slice.call(arguments,2)));};` +
		`if(window.EventSource){var ES=window.EventSource;window.EventSource=function(u,c){return new ES(fix(u),c);};window.EventSource.prototype=ES.prototype;}` +
		`})();</script>`
}

// rewriteRootAbsolute prefixes root-absolute attribute values (src="/x",
// href='/y', action="/z") with the proxy prefix. Protocol-relative URLs
// ("//cdn...") are left untouched.
func rewriteRootAbsolute(s, prefix string) string {
	needles := []string{`src="/`, `href="/`, `action="/`, `src='/`, `href='/`, `action='/`}
	for _, needle := range needles {
		repl := needle[:len(needle)-1] + prefix + "/"
		var b strings.Builder
		i := 0
		for {
			idx := strings.Index(s[i:], needle)
			if idx < 0 {
				b.WriteString(s[i:])
				break
			}
			abs := i + idx
			b.WriteString(s[i:abs])
			after := abs + len(needle)
			if after < len(s) && s[after] == '/' {
				// protocol-relative ("//..."); leave as-is
				b.WriteString(needle)
			} else {
				b.WriteString(repl)
			}
			i = after
		}
		s = b.String()
	}
	return s
}

// stripCSPFrameAncestors removes the frame-ancestors directive from a CSP
// header value so the response can be embedded. Other directives are kept.
func stripCSPFrameAncestors(csp string) string {
	parts := strings.Split(csp, ";")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.HasPrefix(strings.TrimSpace(strings.ToLower(p)), "frame-ancestors") {
			continue
		}
		if strings.TrimSpace(p) == "" {
			continue
		}
		kept = append(kept, strings.TrimSpace(p))
	}
	return strings.Join(kept, "; ")
}

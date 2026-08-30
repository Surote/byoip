package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

// handleRoot serves the index page at exactly "/", and otherwise acts as
// the Referer fallback for requests that "escaped" /p/... (e.g. a page
// requested "/static/app.css" literally instead of a rewritten absolute
// URL). If the Referer names a /p/{scheme}/{host}/... page, redirect the
// request back under that prefix; otherwise 404 with an explanation.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.handleRefererFallback(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderIndex(w, http.StatusOK, "", "", "", "")
}

func (s *Server) handleRefererFallback(w http.ResponseWriter, r *http.Request) {
	ref := r.Header.Get("Referer")
	if ref == "" {
		http.Error(w, "404 not found: "+r.URL.Path+" (no Referer header to recover the proxied host from -- byoip cannot resolve this hostname on its own)", http.StatusNotFound)
		return
	}
	u, err := url.Parse(ref)
	if err != nil {
		http.Error(w, "404 not found: "+r.URL.Path, http.StatusNotFound)
		return
	}
	trimmed := strings.TrimPrefix(u.Path, "/p/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || (parts[0] != "http" && parts[0] != "https") || parts[1] == "" {
		http.Error(w, "404 not found: "+r.URL.Path+" (Referer was not a proxied /p/... page)", http.StatusNotFound)
		return
	}
	newPath := "/p/" + parts[0] + "/" + parts[1] + r.URL.Path
	if r.URL.RawQuery != "" {
		newPath += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, newPath, http.StatusFound)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	ipRaw := strings.TrimSpace(r.FormValue("ip"))
	portRaw := strings.TrimSpace(r.FormValue("port"))

	if err := ValidatePattern(pattern); err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error(), pattern, ipRaw, portRaw)
		return
	}
	ip, err := ValidateIPv4(ipRaw)
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error(), pattern, ipRaw, portRaw)
		return
	}
	port, err := ValidatePort(portRaw)
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error(), pattern, ipRaw, portRaw)
		return
	}

	s.rules.Add(Rule{Pattern: pattern, IP: ip, Port: port})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.rules.Delete(r.FormValue("pattern"))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDiag serves the diagnostics fragment fetched by the index page's
// inline JS after the user hits "Go". It performs its own independent
// fetch (see README: "two fetches" tradeoff) so the /p/ proxy path itself
// stays free of diagnostics bookkeeping.
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if raw == "" {
		_, _ = w.Write([]byte(`<p class="error">missing url parameter</p>`))
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		_, _ = w.Write([]byte(`<p class="error">invalid URL</p>`))
		return
	}
	result := s.runDiagnostics(r.Context(), u)
	if err := diagTmpl.Execute(w, result); err != nil {
		// headers/body already partially written; nothing more to do.
		_ = err
	}
}

// handleProxy serves everything under /p/{scheme}/{host}/{path...}. It
// looks up the host in the rule table, dials the mapped IP directly
// (never real DNS), and streams the response back -- rewriting HTML/CSS
// bodies and the Location header so the browser keeps following /p/...
// links, and stripping headers that would otherwise block iframe embedding.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/p/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || (parts[0] != "http" && parts[0] != "https") || parts[1] == "" {
		http.Error(w, "malformed proxy path; expected /p/{http|https}/{host}/...", http.StatusBadRequest)
		return
	}
	scheme := parts[0]
	host := parts[1]
	path := "/"
	if len(parts) == 3 && parts[2] != "" {
		path = "/" + parts[2]
	}

	rule, ok := s.rules.Match(host)
	if !ok {
		s.renderNoRule(w, host)
		return
	}

	addr, resp, _, err := s.fetchViaRule(r.Context(), rule, r.Method, scheme, host, path, r.URL.RawQuery, r.Header, r.Body, r.ContentLength)
	if err != nil {
		s.renderFetchError(w, rule, addr, host, path, classifyErr(err, addr, s.timeout))
		return
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "" {
		resp.Header.Set("Location", rewriteLocation(loc, scheme))
	}

	outHdr := w.Header()
	for k, vv := range resp.Header {
		if isHopByHop(k) || stripResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			outHdr.Add(k, v)
		}
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/html"):
		data, _ := io.ReadAll(resp.Body)
		data = rewriteHTML(data, scheme)
		outHdr.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	case strings.HasPrefix(ct, "text/css"):
		data, _ := io.ReadAll(resp.Body)
		data = rewriteCSS(data, scheme)
		outHdr.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

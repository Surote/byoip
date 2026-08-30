package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// hopByHopHeaders are stripped in both directions (RFC 7230 §6.1), plus
// Content-Length which is handled explicitly depending on whether the body
// is rewritten.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func isHopByHop(k string) bool {
	return hopByHopHeaders[http.CanonicalHeaderKey(k)]
}

func stripHopByHop(h http.Header) {
	for k := range hopByHopHeaders {
		h.Del(k)
	}
}

// stripResponseHeaders are removed from proxied responses so the target
// page will actually render inside our iframe.
var stripResponseHeaders = map[string]bool{
	"Content-Security-Policy":             true,
	"Content-Security-Policy-Report-Only": true,
	"X-Frame-Options":                     true,
	"Strict-Transport-Security":           true,
	// Without this the browser withholds the Referer path on asset requests,
	// and the root-relative fallback in handleRefererFallback has nothing to
	// recover the proxied host from -- every stylesheet and image 404s.
	"Referrer-Policy": true,
}

// fetchViaRule dials rule.Addr(scheme) directly -- never the hostname --
// while sending the original Host header and (for HTTPS) the original
// hostname as TLS ServerName. This is the whole point of the tool: Host/SNI
// carry the user's hostname, the TCP/TLS connection goes to the mapped IP.
// No real DNS lookup ever happens for the target host.
func (s *Server) fetchViaRule(ctx context.Context, rule Rule, method, scheme, host, path, rawQuery string, hdr http.Header, body io.Reader, contentLength int64) (addr string, resp *http.Response, elapsed time.Duration, err error) {
	addr = rule.Addr(scheme)

	dialer := &net.Dialer{Timeout: s.timeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Ignore the hostname/addr the stdlib would normally dial and
			// connect straight to the rule's IP:port. IPv4 only.
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true, // intentional: this is a lab tool, see README
		},
		TLSHandshakeTimeout:   s.timeout,
		ResponseHeaderTimeout: s.timeout,
		DisableCompression:    true, // ask upstream for identity encoding so bodies are rewritable
		DisableKeepAlives:     true, // each request builds its own throwaway Transport
	}
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // never auto-follow; caller sees the redirect response
		},
	}

	u := &url.URL{Scheme: scheme, Host: host, Path: path, RawQuery: rawQuery}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return addr, nil, 0, err
	}
	req.Host = host
	if hdr != nil {
		req.Header = hdr.Clone()
	} else {
		req.Header = make(http.Header)
	}
	stripHopByHop(req.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.ContentLength = contentLength

	start := time.Now()
	resp, err = client.Do(req)
	elapsed = time.Since(start)
	return addr, resp, elapsed, err
}

// classifyErr turns a fetch error into the actionable, specific message the
// plan calls for ("connection to 10.0.0.5:443 refused" / "timed out after
// 4s"), rather than letting Go's generic wrapped error text leak through.
func classifyErr(err error, addr string, timeout time.Duration) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s connecting to %s", timeout, addr)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("timed out after %s connecting to %s", timeout, addr)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("connection to %s refused", addr)
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return fmt.Sprintf("host %s unreachable", addr)
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return fmt.Sprintf("network unreachable for %s", addr)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return fmt.Sprintf("connection to %s failed: %s", addr, opErr.Err.Error())
	}
	return fmt.Sprintf("connection to %s failed: %s", addr, err.Error())
}

// --- URL rewriting ---
//
// Regex-based, deliberately: golang.org/x/net/html is not stdlib and is
// off-limits. Go's regexp (RE2) does not support backreferences, so quoted
// attributes are matched with separate single/double-quote patterns rather
// than a \1-style backreference to the opening quote.

// toProxyPath rewrites an absolute ("http://host/x", "https://host/x") or
// protocol-relative ("//host/x") URL into our "/p/{scheme}/{host}/x" form.
// Root-relative and page-relative URLs are left untouched (ok=false) --
// they stay under /p/... naturally, or are recovered via the Referer
// fallback in handleRoot.
func toProxyPath(raw string, defaultScheme string) (string, bool) {
	var scheme, rest string
	switch {
	case len(raw) >= 8 && strings.EqualFold(raw[:8], "https://"):
		scheme, rest = "https", raw[8:]
	case len(raw) >= 7 && strings.EqualFold(raw[:7], "http://"):
		scheme, rest = "http", raw[7:]
	case strings.HasPrefix(raw, "//"):
		scheme, rest = defaultScheme, raw[2:]
	default:
		return "", false
	}
	if rest == "" {
		return "", false
	}
	host := rest
	tail := "/"
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
		tail = rest[i:]
	}
	if host == "" {
		return "", false
	}
	// Drop an explicit port from the host component: the rule table's own
	// port override governs what gets dialed, not the URL that happened to
	// be embedded in the page.
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		allDigits := true
		for _, c := range host[idx+1:] {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			host = host[:idx]
		}
	}
	return "/p/" + scheme + "/" + host + tail, true
}

// rewriteLocation rewrites a Location header if it is absolute or
// protocol-relative; relative Location values pass through unchanged.
func rewriteLocation(loc, scheme string) string {
	if newLoc, ok := toProxyPath(loc, scheme); ok {
		return newLoc
	}
	return loc
}

var (
	attrDoubleRe = regexp.MustCompile(`(?is)\b(href|src|action)(\s*=\s*)"([^"]*)"`)
	attrSingleRe = regexp.MustCompile(`(?is)\b(href|src|action)(\s*=\s*)'([^']*)'`)
	srcsetDblRe  = regexp.MustCompile(`(?is)\bsrcset(\s*=\s*)"([^"]*)"`)
	srcsetSglRe  = regexp.MustCompile(`(?is)\bsrcset(\s*=\s*)'([^']*)'`)
	cssURLRe     = regexp.MustCompile(`(?i)url\(\s*(['"]?)([^'")]+)(['"]?)\s*\)`)
)

func rewriteAttrs(body []byte, re *regexp.Regexp, quote byte, scheme string) []byte {
	return re.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := re.FindSubmatch(m)
		attr, eq, val := string(sub[1]), string(sub[2]), string(sub[3])
		newVal, ok := toProxyPath(val, scheme)
		if !ok {
			return m
		}
		return []byte(attr + eq + string(quote) + newVal + string(quote))
	})
}

func rewriteSrcset(body []byte, re *regexp.Regexp, quote byte, scheme string) []byte {
	return re.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := re.FindSubmatch(m)
		eq, val := string(sub[1]), string(sub[2])
		parts := strings.Split(val, ",")
		changed := false
		for i, p := range parts {
			trimmed := strings.TrimSpace(p)
			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				continue
			}
			if newURL, ok := toProxyPath(fields[0], scheme); ok {
				fields[0] = newURL
				changed = true
			}
			parts[i] = strings.Join(fields, " ")
		}
		if !changed {
			return m
		}
		return []byte("srcset" + eq + string(quote) + strings.Join(parts, ", ") + string(quote))
	})
}

// rewriteHTML rewrites absolute/protocol-relative URLs in href/src/action
// and srcset attributes into our /p/{scheme}/{host}/... form.
func rewriteHTML(body []byte, scheme string) []byte {
	body = rewriteAttrs(body, attrDoubleRe, '"', scheme)
	body = rewriteAttrs(body, attrSingleRe, '\'', scheme)
	body = rewriteSrcset(body, srcsetDblRe, '"', scheme)
	body = rewriteSrcset(body, srcsetSglRe, '\'', scheme)
	return body
}

// rewriteCSS rewrites absolute/protocol-relative url(...) references.
func rewriteCSS(body []byte, scheme string) []byte {
	return cssURLRe.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := cssURLRe.FindSubmatch(m)
		openQ, raw, closeQ := string(sub[1]), string(sub[2]), string(sub[3])
		newVal, ok := toProxyPath(raw, scheme)
		if !ok {
			return m
		}
		return []byte("url(" + openQ + newVal + closeQ + ")")
	})
}

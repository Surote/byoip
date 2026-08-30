package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const maxDiagHops = 8

// HeaderKV is a single response header line, for template rendering.
type HeaderKV struct {
	Key   string
	Value string
}

func headerList(h http.Header) []HeaderKV {
	out := make([]HeaderKV, 0, len(h))
	for k, vv := range h {
		for _, v := range vv {
			out = append(out, HeaderKV{Key: k, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// CertInfo summarizes one certificate in a TLS chain for the diagnostics panel.
type CertInfo struct {
	CN        string
	SANs      []string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	Expired   bool
}

func certInfos(state *tls.ConnectionState) []CertInfo {
	if state == nil {
		return nil
	}
	now := time.Now()
	out := make([]CertInfo, 0, len(state.PeerCertificates))
	for _, c := range state.PeerCertificates {
		out = append(out, CertInfo{
			CN:        c.Subject.CommonName,
			SANs:      c.DNSNames,
			Issuer:    c.Issuer.CommonName,
			NotBefore: c.NotBefore,
			NotAfter:  c.NotAfter,
			Expired:   now.After(c.NotAfter) || now.Before(c.NotBefore),
		})
	}
	return out
}

// DiagHop is one fetch in a (possibly redirected) diagnostic run.
type DiagHop struct {
	URL        string
	Rule       string // matched pattern, empty if unmatched
	Addr       string // dialed ip:port
	StatusLine string
	Headers    []HeaderKV
	TLS        []CertInfo
	Duration   time.Duration
	Err        string
}

// DiagResult is the full diagnostic run for one top-level page fetch,
// rendered into the collapsible panel.
type DiagResult struct {
	Hops  []DiagHop
	Total time.Duration
}

// runDiagnostics performs its own independent fetch (see README for why:
// this keeps the /p/ proxy path free of diagnostics bookkeeping), following
// redirects itself -- never through the browser -- so it can record every
// hop: matched rule, dialed address, status, headers, and TLS chain.
func (s *Server) runDiagnostics(ctx context.Context, start *url.URL) DiagResult {
	result := DiagResult{}
	overallStart := time.Now()
	cur := start
	seen := map[string]bool{}

	for i := 0; i < maxDiagHops; i++ {
		key := cur.String()
		if seen[key] {
			result.Hops = append(result.Hops, DiagHop{URL: key, Err: "redirect loop detected"})
			break
		}
		seen[key] = true

		scheme := cur.Scheme
		host := cur.Hostname()
		path := cur.Path
		if path == "" {
			path = "/"
		}

		rule, ok := s.rules.Match(host)
		if !ok {
			result.Hops = append(result.Hops, DiagHop{URL: key, Err: "no mapping rule for " + host})
			break
		}

		addr, resp, elapsed, err := s.fetchViaRule(ctx, rule, http.MethodGet, scheme, host, path, cur.RawQuery, nil, nil, 0)
		hop := DiagHop{URL: key, Rule: rule.Pattern, Addr: addr, Duration: elapsed}
		if err != nil {
			hop.Err = classifyErr(err, addr, s.timeout)
			result.Hops = append(result.Hops, hop)
			break
		}

		hop.StatusLine = resp.Status
		hop.Headers = headerList(resp.Header)
		hop.TLS = certInfos(resp.TLS)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		result.Hops = append(result.Hops, hop)

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			break
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		next, perr := cur.Parse(loc)
		if perr != nil {
			break
		}
		cur = next
	}

	result.Total = time.Since(overallStart)
	return result
}

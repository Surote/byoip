package main

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Rule is a single hostname-pattern -> IPv4[:port] mapping.
type Rule struct {
	Pattern string // exact hostname, or "*.suffix.tld" wildcard
	IP      string // dotted-quad IPv4 address
	Port    int    // 0 means "use the scheme default (80/443)"
}

// PortFor returns the port that should be dialed for the given scheme,
// honoring an explicit rule port override for both http and https.
func (r Rule) PortFor(scheme string) int {
	if r.Port != 0 {
		return r.Port
	}
	if scheme == "https" {
		return 443
	}
	return 80
}

// Addr returns the "ip:port" dial target for the given scheme.
func (r Rule) Addr(scheme string) string {
	return net.JoinHostPort(r.IP, strconv.Itoa(r.PortFor(scheme)))
}

// RuleTable is the global, in-memory, mutex-guarded set of mapping rules.
type RuleTable struct {
	mu    sync.RWMutex
	rules map[string]Rule
}

func NewRuleTable() *RuleTable {
	return &RuleTable{rules: make(map[string]Rule)}
}

// Add inserts or replaces (by exact pattern string) a rule.
func (t *RuleTable) Add(r Rule) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rules[r.Pattern] = r
}

// Delete removes a rule by its exact pattern string.
func (t *RuleTable) Delete(pattern string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.rules, pattern)
}

// All returns every rule, sorted by pattern, for stable UI rendering.
func (t *RuleTable) All() []Rule {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Rule, 0, len(t.rules))
	for _, r := range t.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// Match resolves a host to the best rule: exact match wins outright; among
// wildcard patterns ("*.suffix"), the longest matching suffix wins.
func (t *RuleTable) Match(host string) (Rule, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if r, ok := t.rules[host]; ok {
		return r, true
	}

	var best Rule
	bestLen := -1
	found := false
	for pattern, r := range t.rules {
		if !strings.HasPrefix(pattern, "*.") {
			continue
		}
		suffix := pattern[1:] // ".suffix.tld" (leading dot kept)
		if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
			if len(suffix) > bestLen {
				bestLen = len(suffix)
				best = r
				found = true
			}
		}
	}
	return best, found
}

// --- validation ---

var hostLabelRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !hostLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}

// ValidatePattern checks an exact-hostname or "*.suffix" wildcard pattern.
func ValidatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is required")
	}
	if strings.Contains(pattern, "*") {
		if !strings.HasPrefix(pattern, "*.") {
			return fmt.Errorf(`wildcard pattern must start with "*."`)
		}
		rest := pattern[2:]
		if strings.Contains(rest, "*") {
			return fmt.Errorf("only a single leading wildcard label is supported")
		}
		if !validHostname(rest) {
			return fmt.Errorf("invalid hostname after wildcard: %q", rest)
		}
		return nil
	}
	if !validHostname(pattern) {
		return fmt.Errorf("invalid hostname: %q", pattern)
	}
	return nil
}

// ValidateIPv4 parses and requires a dotted-quad IPv4 address, rejecting
// IPv6 explicitly (project is IPv4-only).
func ValidateIPv4(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("IP address is required")
	}
	parsed := net.ParseIP(raw)
	if parsed == nil {
		return "", fmt.Errorf("invalid IP address: %q", raw)
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", fmt.Errorf("IPv6 addresses are not supported (IPv4 only): %q", raw)
	}
	return v4.String(), nil
}

// ValidatePort parses an optional port string; "" is valid and means "use
// the scheme default".
func ValidatePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port must be a number between 1 and 65535: %q", raw)
	}
	return p, nil
}

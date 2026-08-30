// Command byoip is a small web tool that acts as a visual "curl --resolve":
// enter hostname -> IPv4[:port] mapping rules, then browse a URL through
// the tool, which resolves the hostname to the mapped IP itself (never
// real DNS) and renders the target site inside the page. See PLAN.md and
// README.md for the full design.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Server holds the process-wide, in-memory, shared state: the mapping
// rule table and the configured connect/handshake/header timeout. There is
// deliberately no persistence and no per-session state (see PLAN.md §9).
type Server struct {
	rules   *RuleTable
	timeout time.Duration
}

const defaultTimeout = 4 * time.Second

func loadTimeout() time.Duration {
	v := os.Getenv("TIMEOUT_SECONDS")
	if v == "" {
		return defaultTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		log.Printf("invalid TIMEOUT_SECONDS=%q, using default %s", v, defaultTimeout)
		return defaultTimeout
	}
	return time.Duration(secs) * time.Second
}

func main() {
	s := &Server{
		rules:   NewRuleTable(),
		timeout: loadTimeout(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/rules", s.handleAddRule)
	mux.HandleFunc("/rules/delete", s.handleDeleteRule)
	mux.HandleFunc("/api/diag", s.handleDiag)
	mux.HandleFunc("/p/", s.handleProxy)

	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("byoip listening on %s (fetch timeout=%s)", addr, s.timeout)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

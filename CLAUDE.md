# byoip

Visual `curl --resolve`: an in-memory table maps hostname patterns to IPv4 addresses; the tool proxies and renders the target site as if DNS pointed there. The spec is `PLAN.md` — its decisions are settled with the owner, and §9 lists hard non-goals (persistence, auth, IPv6, JS rewriting, WebSockets). §8 holds the acceptance criteria.

## Invariants

- **Stdlib only.** The Dockerfile builds with `GOPROXY=off`, so a third-party import fails the disconnected build. Solve every problem with the standard library.
- **Air-gapped end to end.** UI assets ship inside the binary via `go:embed`; every asset stays embedded and every URL in the UI stays self-hosted.
- **Host/SNI preservation is the product.** Proxied connections dial the mapped `IP:port` while sending the user's hostname as the `Host` header and TLS `ServerName` (`InsecureSkipVerify` is deliberate). Target hostnames are resolved only through the rule table — the real resolver stays out of the request path.
- **OpenShift restricted-v2.** Runs as an arbitrary UID, writes only under `/tmp`, listens on 8080 — the port is fixed by the `deploy/` container/Service/Route contract.

## Verify

`GOPROXY=off go build ./...` and `go vet ./...` must both pass before any change is done.

## Layout

Flat `package main` (`main.go`, `rules.go`, `proxy.go`, `diag.go`, `handlers.go`, `ui.go`). Keep it flat — this is a lab tool; new code joins an existing file or adds a sibling at the root.

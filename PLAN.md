# byoip — Implementation Plan

Work order for an implementing agent. All design decisions below are **settled with the user** — do not re-litigate them. If something is genuinely ambiguous, prefer the simplest interpretation consistent with the constraints.

## 1. What this is

A small web tool that acts as a **visual `curl --resolve`**. A tester enters DNS mapping rules
(`*.apps.swongpai.local → 10.0.0.5`) and then browses URLs like
`https://console.apps.swongpai.local/` **through the tool**, which resolves the hostname to the
mapped IP itself (no real DNS involved) and renders the target site inside the page.

Primary use case: verifying that an OpenShift router / load balancer VIP answers correctly for a
wildcard domain **before** real DNS records exist. Built generally enough that any hostname→IP
works.

Runs inside OpenShift in a **fully disconnected (air-gapped) environment**. Zero internet
dependency at build time and at runtime is a hard requirement.

## 2. Hard constraints

- **Go, standard library only.** No third-party modules. No `go get` beyond stdlib. `go.mod` with no requirements.
- **No network access during build or runtime** other than to user-specified target IPs. No CDN links, no Google Fonts, no npm — the tool's own UI assets are embedded in the binary (use `embed` or string constants).
- **OpenShift-safe container**: runs as non-root with an **arbitrary UID** (no `USER 1001`-only assumptions, no writes outside `/tmp`, no chown tricks needed), listens on **8080**, plain HTTP (TLS for users is terminated by an edge Route).
- **IPv4 only.** Reject IPv6 addresses in rules with a clear validation error.
- All state is **in-memory and global** (shared by all users of the pod). No persistence. Pod restart clears rules — that is acceptable and documented.

## 3. Repository layout to produce

```
byoip/
├── PLAN.md                    (this file)
├── README.md
├── go.mod                     (module byoip, stdlib only)
├── main.go                    (or split into a few files in package main — keep it flat)
├── Dockerfile
└── deploy/
    ├── deployment.yaml
    ├── service.yaml
    └── route.yaml
```

Keep the Go code in package `main`, a handful of files at most (e.g. `main.go`, `proxy.go`,
`rules.go`, `ui.go`). This is a lab utility, not a product — no internal packages, no
interfaces-for-testability ceremony.

## 4. Functional specification

### 4.1 Mapping rules

- A rule is `pattern → IPv4[:port]`.
  - `pattern`: exact hostname (`console.swongpai.local`) or wildcard (`*.apps.swongpai.local`).
    Wildcard matches one-or-more labels on the left (`a.apps.swongpai.local` and
    `x.y.apps.swongpai.local` both match). Exact match wins over wildcard; longest wildcard
    suffix wins among wildcards.
  - `IPv4`: dotted-quad, validated (use `net.ParseIP`, reject if `To4() == nil`).
  - optional `:port` (1–65535): overrides the default target port for **both** schemes
    (default 80 for `http`, 443 for `https`).
- Global in-memory table guarded by a mutex. Add / delete via the UI (simple POST forms).
  Duplicate pattern replaces the existing rule.

### 4.2 Proxy

- Proxied URLs have the shape **`/p/{scheme}/{host}/{path...}?{query}`**, e.g.
  `/p/https/console.apps.swongpai.local/settings`.
- Resolution: look up `{host}` in the rules table.
  - **Match** → dial `IP:port` directly (custom `DialContext` that ignores the hostname and
    connects to the rule's IP), but send the original `Host` header and, for HTTPS, the original
    hostname as **SNI** (`tls.Config{ServerName: host, InsecureSkipVerify: true}`). This is the
    whole point of the tool — Host/SNI must be the user's hostname, connection must go to the
    mapped IP.
  - **No match** → do NOT attempt real DNS. Serve an error page: *"No mapping rule for
    `cdn.example.com`"* with an inline form pre-filled to add a rule for that host, and a link
    back. Air-gapped means "silently broken" wastes debugging time; make it actionable.
- Use `net/http/httputil.ReverseProxy` or a hand-rolled round trip — either is fine, but you need
  access to the response for rewriting and diagnostics, so a hand-rolled
  `http.Transport`-based fetch is likely simpler than fighting `ReverseProxy` hooks.
- **Redirect handling**: do not auto-follow. Rewrite `Location` headers pointing at any
  `http(s)://host/...` into `/p/{scheme}/{host}/...` and let the browser follow — this keeps the
  address path honest and makes each hop visible in diagnostics. Relative redirects need no
  rewriting (they stay under `/p/...` naturally).
- **HTML rewriting** (only when `Content-Type` is `text/html`): rewrite absolute URLs
  (`http://…`, `https://…`, and protocol-relative `//host/…`) in common attributes
  (`href`, `src`, `action`, `srcset`) to `/p/{scheme}/{host}/…`. Regex-based rewriting over the
  body is acceptable; do not import an HTML parser beyond stdlib (`golang.org/x/net/html` is
  NOT stdlib — do not use it). Root-relative URLs (`/static/app.css`) are left alone — see
  Referer fallback.
- **Referer fallback**: requests landing outside `/p/` (e.g. the page requested
  `/static/app.css` literally) are recovered by parsing the `Referer` header — if it contains
  `/p/{scheme}/{host}/`, issue a redirect to `/p/{scheme}/{host}/static/app.css`. If no usable
  Referer, 404 with a short explanation. This makes most sites render without aggressive
  rewriting.
- Also rewrite CSS bodies' `url(...)` absolute references (same regex approach) — cheap and
  catches common breakage. Do not attempt JavaScript rewriting; document the limitation.
- **Compression**: send `Accept-Encoding: identity` upstream (set `Transport.DisableCompression`
  or strip the header) so bodies arrive un-gzipped and rewritable. Strip upstream
  `Content-Length` after rewriting (use chunked) and strip `Content-Security-Policy`,
  `X-Frame-Options`, and `Strict-Transport-Security` response headers so the page renders inside
  the iframe.
- **Cookies**: pass through as-is (best effort; domain-scoped cookies across proxied hosts are a
  known limitation, document it).
- **Timeouts**: connect + response-header timeout of **4 seconds** by default, overridable with
  env `TIMEOUT_SECONDS`. On failure serve a fail-fast error page stating exactly what happened:
  `connection to 10.0.0.5:443 refused` / `timed out after 4s`, plus the rule that matched.

### 4.3 Diagnostics

Capture per proxied **top-level page fetch** (not every asset) and show in a collapsible panel:

- matched rule (pattern) and the `IP:port` actually dialed
- HTTP status line and response headers
- redirect chain so far (derivable from the visible hops; keeping a small ring buffer of the
  last N fetches keyed by host is fine)
- TLS certificate chain when HTTPS: for each cert — Subject CN, SANs, Issuer, NotBefore/NotAfter
  (flag expired in red) — taken from `tls.ConnectionState.PeerCertificates`
- total response time

Implementation suggestion: the main UI fetches `/api/diag?url=...` after loading the iframe, or
simpler — the tool fetches the page itself when the user hits "Go", records diagnostics, and the
iframe then loads `/p/...` (second fetch). Two fetches of the same URL is an acceptable cost for
a lab tool; pick whichever is simpler and note the choice in the README.

### 4.4 UI

Single page at `/`, server-rendered with `html/template`, embedded CSS (`embed` package), **no
JavaScript frameworks, no external references of any kind**. A few lines of vanilla inline JS
(collapsible panel) are fine.

Layout, top to bottom:

1. **Rules table**: pattern / IP:port / delete button; add-rule form (pattern, IP, optional port).
   Validation errors shown inline (bad IPv4, bad pattern, IPv6 rejected).
2. **URL bar**: text input (`https://console.apps.swongpai.local/`) + Go button. Accepts full
   URLs; bare hostnames default to `https://`.
3. **Diagnostics panel** (collapsible, populated after a fetch).
4. **Rendered site** in an `<iframe>` filling the rest of the viewport, `src=/p/...`.

Keep the design clean and minimal — a small amount of embedded CSS, system font stack, works in
both light and dark terminal-adjacent tastes. No branding.

## 5. Dockerfile

Multi-stage, fully parameterized for disconnected mirrors:

```dockerfile
ARG BUILD_IMAGE=registry.access.redhat.com/ubi9/go-toolset:latest
ARG RUNTIME_IMAGE=registry.access.redhat.com/ubi9/ubi-micro:latest

FROM ${BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# stdlib only — no go mod download needed; forbid network to prove it
ENV GOFLAGS=-mod=mod GOPROXY=off CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /tmp/byoip .

FROM ${RUNTIME_IMAGE}
COPY --from=build /tmp/byoip /usr/local/bin/byoip
EXPOSE 8080
# arbitrary-UID safe: binary + no writable state needed
USER 1001
ENTRYPOINT ["/usr/local/bin/byoip"]
```

Requirements (adjust the sketch above as needed, keep these properties):

- `GOPROXY=off` so the build **fails loudly** if any non-stdlib dependency sneaks in.
- `CGO_ENABLED=0` → static binary → `ubi-micro` runtime works.
- Both images overridable via `--build-arg` for internal mirrors.
- Builds identically with `podman build`, `docker build`, and OpenShift `BuildConfig`.
- No `RUN dnf install`, no curl, no downloads of any kind.

## 6. OpenShift manifests (`deploy/`)

- **deployment.yaml**: 1 replica, image placeholder `image-registry.example.com/byoip:latest`
  (comment telling the user to change it), port 8080, env `TIMEOUT_SECONDS` shown but commented,
  readiness + liveness probes on `GET /healthz` (implement the endpoint — returns 200 "ok"),
  resources requests/limits modest (e.g. 50m/128Mi requests, 500m/256Mi limits),
  `securityContext` compatible with the `restricted-v2` SCC (no privilege, no fixed UID,
  `allowPrivilegeEscalation: false`, drop ALL capabilities, seccomp `RuntimeDefault`).
- **service.yaml**: ClusterIP, port 8080.
- **route.yaml**: edge-terminated TLS, `insecureEdgeTerminationPolicy: Redirect`, host left for
  the cluster default (placeholder comment for a custom host).
- All three must apply cleanly with a single `oc apply -f deploy/`.

## 7. README.md

Short and practical: what it does (one paragraph + the `curl --resolve` analogy), screenshot
placeholder, how to build (podman, with `--build-arg` mirror overrides), how to run locally
(`podman run -p 8080:8080`), how to deploy (`oc apply -f deploy/`), configuration table
(`TIMEOUT_SECONDS`), and a **Limitations** section (state resets on restart; JS-heavy SPAs that
construct URLs in code may not fully render; cookies across proxied hosts are best-effort;
IPv4 only; TLS verification intentionally disabled — this is a lab tool, do not expose it
publicly).

## 8. Acceptance criteria

1. `go build ./...` succeeds with `GOPROXY=off` (proves stdlib-only).
2. `go vet ./...` clean.
3. `podman build .` succeeds on a machine with **networking limited to the base-image pull**.
4. Running locally: add rule `*.apps.example.test → <IP of a local test nginx>`, browse
   `http://foo.apps.example.test/` through the tool → nginx page renders in the iframe, and the
   nginx access log shows `Host: foo.apps.example.test`.
5. HTTPS target with self-signed cert renders, and diagnostics show the cert's CN/SANs/expiry.
6. Unmapped host produces the "no mapping rule" page with a pre-filled add-rule form.
7. Rule pointing at a dead IP fails in ~4s with a clear refused/timeout error page (and honors
   `TIMEOUT_SECONDS=1`).
8. IPv6 input (`fd00::5`) is rejected with a visible validation message.
9. Port-override rule (`→ 10.0.0.5:8443`) dials 8443 for both schemes.
10. Container runs as a random UID: `podman run --user 12345:0 ...` works.
11. `oc apply -f deploy/` produces a Running pod passing probes under `restricted-v2` SCC
    (verify manifests are at least schema-valid via `oc apply --dry-run=client` if no cluster
    is available).

## 9. Out of scope (do not build)

- Persistence of rules, auth/login, multi-tenancy per-session state
- Headless-browser/screenshot rendering
- JavaScript rewriting, WebSocket proxying
- IPv6, HTTP/2 to upstream (default `http.Transport` behavior is fine), metrics/observability

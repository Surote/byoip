package main

import (
	_ "embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed style.css
var styleCSS string

// --- templates ---
//
// All pages are server-rendered html/template with the CSS embedded inline
// (via the //go:embed above) -- zero external references, no CDN, no
// fonts, no JS frameworks. The handful of inline <script> lines are plain
// vanilla JS for the collapsible diagnostics panel and translating the URL
// bar into a /p/... path.

const indexTmplSrc = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>byoip</title>
<style>{{.CSS}}</style>
</head>
<body>
<header>
  <h1>byoip <span class="tag">a visual curl --resolve</span></h1>
</header>

<section class="rules">
  <h2>Mapping rules</h2>
  {{if .RuleError}}<p class="error">{{.RuleError}}</p>{{end}}
  <table>
    <thead><tr><th>Pattern</th><th>Target</th><th></th></tr></thead>
    <tbody>
    {{range .Rules}}
      <tr>
        <td><code>{{.Pattern}}</code></td>
        <td><code>{{.IP}}{{if .Port}}:{{.Port}}{{end}}</code></td>
        <td>
          <form method="post" action="/rules/delete" class="inline">
            <input type="hidden" name="pattern" value="{{.Pattern}}">
            <button type="submit">delete</button>
          </form>
        </td>
      </tr>
    {{else}}
      <tr><td colspan="3" class="muted">no rules yet</td></tr>
    {{end}}
    </tbody>
  </table>

  <form method="post" action="/rules" class="add-rule">
    <input type="text" name="pattern" placeholder="*.apps.example.local or host.example.local" value="{{.Pattern}}" required>
    <input type="text" name="ip" placeholder="10.0.0.5" value="{{.IP}}" required>
    <input type="text" name="port" placeholder="port (optional)" value="{{.Port}}">
    <button type="submit">add rule</button>
  </form>
</section>

<section class="urlbar">
  <h2>Browse through byoip</h2>
  <div class="urlbar">
    <input type="text" id="urlInput" placeholder="https://console.apps.example.local/">
    <button id="goBtn" type="button">Go</button>
  </div>
</section>

<section class="diag">
  <button id="diagToggle" type="button">Diagnostics &#9656;</button>
  <div id="diagPanel" class="diag-panel" hidden></div>
</section>

<iframe id="viewer" name="viewer" src="about:blank"></iframe>

<script>
(function() {
  var goBtn = document.getElementById('goBtn');
  var input = document.getElementById('urlInput');
  var iframe = document.getElementById('viewer');
  var diagPanel = document.getElementById('diagPanel');
  var diagToggle = document.getElementById('diagToggle');

  function normalize(raw) {
    raw = raw.trim();
    if (!raw) { return null; }
    if (!/^https?:\/\//i.test(raw)) { raw = 'https://' + raw; }
    return raw;
  }

  function proxify(u) {
    var a = new URL(u);
    var scheme = a.protocol.replace(':', '');
    return '/p/' + scheme + '/' + a.host + a.pathname + a.search;
  }

  function go() {
    var raw = normalize(input.value);
    if (!raw) { return; }
    var target;
    try { target = proxify(raw); } catch (e) { return; }
    iframe.src = target;
    diagPanel.removeAttribute('hidden');
    diagToggle.textContent = 'Diagnostics ▾';
    diagPanel.innerHTML = '<p class="muted">loading diagnostics&hellip;</p>';
    fetch('/api/diag?url=' + encodeURIComponent(raw))
      .then(function(r) { return r.text(); })
      .then(function(html) { diagPanel.innerHTML = html; })
      .catch(function() { diagPanel.innerHTML = '<p class="error">diagnostics fetch failed</p>'; });
  }

  goBtn.addEventListener('click', go);
  input.addEventListener('keydown', function(e) { if (e.key === 'Enter') { go(); } });

  diagToggle.addEventListener('click', function() {
    var hidden = diagPanel.hasAttribute('hidden');
    if (hidden) {
      diagPanel.removeAttribute('hidden');
      diagToggle.textContent = 'Diagnostics ▾';
    } else {
      diagPanel.setAttribute('hidden', '');
      diagToggle.textContent = 'Diagnostics ▶';
    }
  });
})();
</script>
</body>
</html>`

const noRuleTmplSrc = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>no mapping rule</title><style>{{.CSS}}</style></head>
<body>
<div class="errorpage">
  <h1>No mapping rule for <code>{{.Host}}</code></h1>
  <p>byoip never performs a real DNS lookup for a proxied hostname &mdash; add a mapping rule below, then use the URL bar again.</p>
  <form method="post" action="/rules" target="_top" class="add-rule">
    <input type="text" name="pattern" value="{{.Host}}" required>
    <input type="text" name="ip" placeholder="10.0.0.5" required>
    <input type="text" name="port" placeholder="port (optional)">
    <button type="submit">add rule</button>
  </form>
  <p><a href="/" target="_top">&larr; back to byoip</a></p>
</div>
</body>
</html>`

const fetchErrorTmplSrc = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>fetch failed</title><style>{{.CSS}}</style></head>
<body>
<div class="errorpage">
  <h1>Fetch failed</h1>
  <p class="error">{{.Message}}</p>
  <table class="kv">
    <tr><td>host</td><td><code>{{.Host}}</code></td></tr>
    <tr><td>matched rule</td><td><code>{{.Pattern}}</code></td></tr>
    <tr><td>dialed</td><td><code>{{.Addr}}</code></td></tr>
    <tr><td>path</td><td><code>{{.Path}}</code></td></tr>
  </table>
  <p><a href="/" target="_top">&larr; back to byoip</a></p>
</div>
</body>
</html>`

const diagTmplSrc = `{{if not .Hops}}<p class="muted">no diagnostics</p>{{end}}
<p class="muted">total: {{.Total}}</p>
{{range $i, $h := .Hops}}
<div class="hop">
  <h3>hop {{inc $i}}: {{$h.URL}}</h3>
  {{if $h.Rule}}<p>matched rule <code>{{$h.Rule}}</code> &rarr; dialed <code>{{$h.Addr}}</code></p>{{end}}
  {{if $h.StatusLine}}<p>status <code>{{$h.StatusLine}}</code> in {{$h.Duration}}</p>{{end}}
  {{if $h.Err}}<p class="error">{{$h.Err}}</p>{{end}}
  {{if $h.Headers}}
  <details>
    <summary>response headers</summary>
    <table class="headers">
    {{range $h.Headers}}<tr><td>{{.Key}}</td><td>{{.Value}}</td></tr>{{end}}
    </table>
  </details>
  {{end}}
  {{if $h.TLS}}
  <details open>
    <summary>TLS certificate chain</summary>
    {{range $h.TLS}}
    <div class="cert{{if .Expired}} expired{{end}}">
      <p>CN: {{.CN}}</p>
      <p>SANs: {{range .SANs}}{{.}} {{else}}(none){{end}}</p>
      <p>Issuer: {{.Issuer}}</p>
      <p>Valid: {{.NotBefore}} &ndash; {{.NotAfter}}{{if .Expired}} <strong>(EXPIRED / NOT YET VALID)</strong>{{end}}</p>
    </div>
    {{end}}
  </details>
  {{end}}
</div>
{{end}}`

var (
	indexTmpl      = template.Must(template.New("index").Parse(indexTmplSrc))
	noRuleTmpl     = template.Must(template.New("norule").Parse(noRuleTmplSrc))
	fetchErrorTmpl = template.Must(template.New("fetcherr").Parse(fetchErrorTmplSrc))
	diagTmpl       = template.Must(template.New("diag").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
	}).Parse(diagTmplSrc))
)

type indexData struct {
	CSS       template.CSS
	Rules     []Rule
	RuleError string
	Pattern   string
	IP        string
	Port      string
}

func (s *Server) renderIndex(w http.ResponseWriter, status int, ruleErr, pattern, ip, port string) {
	data := indexData{
		CSS:       template.CSS(styleCSS),
		Rules:     s.rules.All(),
		RuleError: ruleErr,
		Pattern:   pattern,
		IP:        ip,
		Port:      port,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("render index: %v", err)
	}
}

type noRuleData struct {
	CSS  template.CSS
	Host string
}

func (s *Server) renderNoRule(w http.ResponseWriter, host string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	if err := noRuleTmpl.Execute(w, noRuleData{CSS: template.CSS(styleCSS), Host: host}); err != nil {
		log.Printf("render norule: %v", err)
	}
}

type fetchErrorData struct {
	CSS     template.CSS
	Message string
	Host    string
	Pattern string
	Addr    string
	Path    string
}

func (s *Server) renderFetchError(w http.ResponseWriter, rule Rule, addr, host, path, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	data := fetchErrorData{
		CSS:     template.CSS(styleCSS),
		Message: message,
		Host:    host,
		Pattern: rule.Pattern,
		Addr:    addr,
		Path:    path,
	}
	if err := fetchErrorTmpl.Execute(w, data); err != nil {
		log.Printf("render fetcherr: %v", err)
	}
}

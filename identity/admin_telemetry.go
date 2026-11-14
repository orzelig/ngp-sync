package main

// Operator-only read endpoint for the anonymous telemetry store (the opt-in
// settings-reset reports collected by telemetry.go). It decodes the stored
// ChromeResetReport protobufs server-side and renders them as an HTML page and
// a JSON feed.
//
// This listener is served on a SEPARATE port (ADMIN_ADDR, default :8301) that
// Caddy does not proxy and the VM firewall does not expose to the internet. It
// is reached only through a Google IAP tunnel:
//
//	gcloud compute start-iap-tunnel ngp-sync 8301 \
//	  --local-host-port=localhost:8301 --zone us-central1-a
//	open http://localhost:8301/
//
// There is deliberately no app-level password here: access is gated entirely by
// IAP (Google identity + the firewall rule restricting :8301 to the IAP range).
// Keep it off the public mux — it exposes users' settings data (startup URLs,
// homepage, installed extensions), which the public /t/reset sink never returns.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"ngp.one/identity/internal/resetreport"
)

// maxHTMLRows caps how many reports the HTML page renders (the most recent
// ones); the full set is always available at /reports.json.
const maxHTMLRows = 2000

type telemetryAdmin struct {
	path string
	tmpl *template.Template
}

func newTelemetryAdmin(path string) *telemetryAdmin {
	return &telemetryAdmin{
		path: path,
		tmpl: template.Must(template.New("reports").Parse(reportsHTML)),
	}
}

// startTelemetryAdmin runs the admin listener; intended to be called in a
// goroutine. An empty addr disables it.
func startTelemetryAdmin(addr, path string) {
	if addr == "" {
		return
	}
	ta := newTelemetryAdmin(path)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", ta.handleHTML)
	mux.HandleFunc("GET /reports", ta.handleHTML)
	mux.HandleFunc("GET /reports.json", ta.handleJSON)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second, // decoding a large store can take a moment
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("telemetry admin listening on %s (IAP-gated; reads %s)", addr, path)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("telemetry admin server stopped: %v", err)
	}
}

type extView struct{ ID, Name string }

type reportView struct {
	Idx                 int
	Time                string
	ResetRequestOrigin  string
	StartupType         string
	StartupURLs         []string
	Homepage            string
	HomepageNTP         string
	ShowHomeButton      string
	DefaultSearchEngine string
	Extensions          []extView
	Shortcuts           []string
	GUID                string
	Unknown             []string
}

type badView struct {
	Idx     int
	Time    string
	Err     string
	Len     int
	Preview string
}

type pageData struct {
	Path        string
	Generated   string
	Total       int
	Decoded     int
	Bad         int
	Shown       int
	Truncated   bool
	MissingFile bool
	Reports     []reportView
	Bads        []badView
}

func boolStr(p *bool) string {
	if p == nil {
		return ""
	}
	if *p {
		return "true"
	}
	return "false"
}

// collect reads and decodes the store. A missing file is not an error (no
// reports yet); any other open failure is returned.
func (ta *telemetryAdmin) collect() (*pageData, error) {
	pd := &pageData{Path: ta.path, Generated: time.Now().UTC().Format(time.RFC3339)}

	f, err := os.Open(ta.path)
	if err != nil {
		if os.IsNotExist(err) {
			pd.MissingFile = true
			return pd, nil
		}
		return nil, err
	}
	defer f.Close()

	var reports []reportView
	var bads []badView
	s := resetreport.Safe
	err = resetreport.ScanStore(f, func(rec resetreport.Record) {
		pd.Total++
		if rec.Err != nil {
			pd.Bad++
			bads = append(bads, badView{
				Idx: pd.Total, Time: tsUTC(rec.TS), Err: rec.Err.Error(),
				Len: rec.Len, Preview: resetreport.HexPreview(rec.Raw, 48),
			})
			return
		}
		pd.Decoded++
		r := rec.Report
		var exts []extView
		for _, e := range r.Extensions {
			exts = append(exts, extView{ID: s(e.ID), Name: s(e.Name)})
		}
		urls := make([]string, len(r.StartupURLs))
		for i, u := range r.StartupURLs {
			urls[i] = s(u)
		}
		shorts := make([]string, len(r.Shortcuts))
		for i, sc := range r.Shortcuts {
			shorts[i] = s(sc)
		}
		reports = append(reports, reportView{
			Idx: pd.Total, Time: tsUTC(rec.TS),
			ResetRequestOrigin:  r.ResetRequestOrigin,
			StartupType:         r.StartupType,
			StartupURLs:         urls,
			Homepage:            s(r.Homepage),
			HomepageNTP:         boolStr(r.HomepageIsNTP),
			ShowHomeButton:      boolStr(r.ShowHomeButton),
			DefaultSearchEngine: s(r.DefaultSearchEngine),
			Extensions:          exts,
			Shortcuts:           shorts,
			GUID:                s(r.GUID),
			Unknown:             r.Unknown,
		})
	})
	if err != nil {
		return nil, err
	}

	// Most recent first for display.
	for i, j := 0, len(reports)-1; i < j; i, j = i+1, j-1 {
		reports[i], reports[j] = reports[j], reports[i]
	}
	for i, j := 0, len(bads)-1; i < j; i, j = i+1, j-1 {
		bads[i], bads[j] = bads[j], bads[i]
	}
	pd.Shown = len(reports)
	if len(reports) > maxHTMLRows {
		reports = reports[:maxHTMLRows]
		pd.Shown = maxHTMLRows
		pd.Truncated = true
	}
	pd.Reports = reports
	pd.Bads = bads
	return pd, nil
}

func (ta *telemetryAdmin) handleHTML(w http.ResponseWriter, _ *http.Request) {
	pd, err := ta.collect()
	if err != nil {
		http.Error(w, "read telemetry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := ta.tmpl.Execute(w, pd); err != nil {
		log.Printf("telemetry admin: template: %v", err)
	}
}

// handleJSON streams every decoded report as ndjson (one object per line),
// followed by nothing else, so it pipes cleanly into jq.
func (ta *telemetryAdmin) handleJSON(w http.ResponseWriter, _ *http.Request) {
	f, err := os.Open(ta.path)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			return // no reports yet: empty body
		}
		http.Error(w, "read telemetry: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	enc := json.NewEncoder(bw)
	_ = resetreport.ScanStore(f, func(rec resetreport.Record) {
		if rec.Err != nil {
			_ = enc.Encode(map[string]any{
				"ts": rec.TS, "time": tsUTC(rec.TS), "error": rec.Err.Error(),
				"len": rec.Len, "preview_hex": resetreport.HexPreview(rec.Raw, 64),
			})
			return
		}
		_ = enc.Encode(struct {
			TS     int64               `json:"ts"`
			Time   string              `json:"time"`
			Report *resetreport.Report `json:"report"`
		}{rec.TS, tsUTC(rec.TS), rec.Report})
	})
}

func tsUTC(ts int64) string {
	if ts <= 0 {
		return "(no timestamp)"
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04:05 MST")
}

const reportsHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow, noarchive">
<title>NGP telemetry</title>
<style>
  :root { --orange:#EE5A05; --ink:#1b1b1f; --mut:#6b6b74; --bg:#fafafa; --card:#fff; --line:#e6e6ea; }
  @media (prefers-color-scheme: dark) {
    :root { --ink:#ececf0; --mut:#9a9aa4; --bg:#131316; --card:#1b1b20; --line:#2a2a31; }
  }
  * { box-sizing:border-box; }
  body { margin:0; font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;
         background:var(--bg); color:var(--ink); }
  main { max-width:960px; margin:0 auto; padding:32px 20px 72px; }
  h1 { font-size:22px; margin:0 0 4px; }
  .sub { color:var(--mut); font-size:13px; margin-bottom:20px; }
  .stats { display:flex; flex-wrap:wrap; gap:10px; margin-bottom:24px; }
  .stat { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:10px 14px; }
  .stat b { font-size:20px; display:block; }
  .stat span { color:var(--mut); font-size:12px; }
  .note { background:var(--card); border:1px solid var(--line); border-left:3px solid var(--orange);
          border-radius:8px; padding:10px 14px; color:var(--mut); font-size:13px; margin-bottom:20px; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:14px 16px; margin-bottom:14px; }
  .card h2 { font-size:13px; font-weight:600; color:var(--mut); margin:0 0 10px; font-variant-numeric:tabular-nums; }
  table { width:100%; border-collapse:collapse; }
  td { padding:3px 0; vertical-align:top; }
  td.k { color:var(--mut); width:150px; padding-right:12px; white-space:nowrap; }
  td.v { word-break:break-word; }
  ul { margin:0; padding-left:18px; }
  code { font:13px ui-monospace,SFMono-Regular,Menlo,monospace; word-break:break-all; }
  .bad code { color:var(--orange); }
  a { color:var(--orange); }
  .empty { color:var(--mut); padding:40px 0; text-align:center; }
</style></head><body><main>
<h1>NGP telemetry</h1>
<div class="sub">Opt-in settings-reset reports · generated {{.Generated}} · <a href="/reports.json">reports.json</a></div>

{{if .MissingFile}}
  <div class="empty">No reports yet — the store ({{.Path}}) does not exist.</div>
{{else}}
  <div class="stats">
    <div class="stat"><b>{{.Total}}</b><span>records</span></div>
    <div class="stat"><b>{{.Decoded}}</b><span>decoded</span></div>
    <div class="stat"><b>{{.Bad}}</b><span>unparseable</span></div>
  </div>
  {{if .Truncated}}<div class="note">Showing the {{.Shown}} most recent reports. The full set is at <a href="/reports.json">/reports.json</a>.</div>{{end}}
  {{if eq .Total 0}}<div class="empty">The store is empty.</div>{{end}}

  {{range .Reports}}
  <div class="card">
    <h2>#{{.Idx}} · {{.Time}}</h2>
    <table>
      {{if .ResetRequestOrigin}}<tr><td class="k">reset origin</td><td class="v">{{.ResetRequestOrigin}}</td></tr>{{end}}
      {{if .StartupType}}<tr><td class="k">startup type</td><td class="v">{{.StartupType}}</td></tr>{{end}}
      {{if .StartupURLs}}<tr><td class="k">startup URLs</td><td class="v"><ul>{{range .StartupURLs}}<li><code>{{.}}</code></li>{{end}}</ul></td></tr>{{end}}
      {{if .Homepage}}<tr><td class="k">homepage</td><td class="v"><code>{{.Homepage}}</code>{{if .HomepageNTP}} (new tab page: {{.HomepageNTP}}){{end}}</td></tr>{{end}}
      {{if .ShowHomeButton}}<tr><td class="k">show home button</td><td class="v">{{.ShowHomeButton}}</td></tr>{{end}}
      {{if .DefaultSearchEngine}}<tr><td class="k">search engine</td><td class="v"><code>{{.DefaultSearchEngine}}</code></td></tr>{{end}}
      {{if .Extensions}}<tr><td class="k">extensions ({{len .Extensions}})</td><td class="v"><ul>{{range .Extensions}}<li><code>{{.ID}}</code> — {{.Name}}</li>{{end}}</ul></td></tr>{{end}}
      {{if .Shortcuts}}<tr><td class="k">shortcuts ({{len .Shortcuts}})</td><td class="v"><ul>{{range .Shortcuts}}<li><code>{{.}}</code></li>{{end}}</ul></td></tr>{{end}}
      {{if .GUID}}<tr><td class="k">guid</td><td class="v"><code>{{.GUID}}</code></td></tr>{{end}}
      {{range .Unknown}}<tr><td class="k">unknown</td><td class="v"><code>{{.}}</code></td></tr>{{end}}
    </table>
  </div>
  {{end}}

  {{if .Bads}}
  <h2 style="color:var(--mut);font-size:13px;margin:24px 0 10px">Unparseable ({{.Bad}})</h2>
  {{range .Bads}}
  <div class="card bad">
    <h2>#{{.Idx}} · {{.Time}} · {{.Len}} bytes</h2>
    <table>
      <tr><td class="k">error</td><td class="v">{{.Err}}</td></tr>
      <tr><td class="k">preview</td><td class="v"><code>{{.Preview}}</code></td></tr>
    </table>
  </div>
  {{end}}
  {{end}}
{{end}}
</main></body></html>`

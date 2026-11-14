// Command telemetry-decode turns the anonymous NGP telemetry store into
// readable reports.
//
// The store (/data/telemetry.jsonl on the prod VM, written by telemetry.go) is
// one JSON object per line: {"ts":<unix secs>,"len":<raw bytes>,"b64":"..."}.
// b64 is the base64 of the exact body the NGP browser POSTed to id.ngp.one/t/reset
// — a serialized reset_report.ChromeResetReport protobuf. This tool decodes
// each line into labelled fields; the decoding itself lives in the shared
// internal/resetreport package (also used by the identity service's read page).
//
// Usage:
//
//	cd identity && go run ./cmd/telemetry-decode [flags] [file]
//
// With no file (or "-") it reads stdin, so it pipes straight off the VM:
//
//	./scripts/telemetry-fetch.sh | (cd identity && go run ./cmd/telemetry-decode)
//	./scripts/telemetry-read.sh            # fetch + decode in one shot
//	./scripts/telemetry-read.sh -json | jq # structured, for aggregation
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"ngp.one/identity/internal/resetreport"
)

func main() {
	jsonOut := flag.Bool("json", false, "emit one JSON object per report (ndjson) instead of text")
	showUnknown := flag.Bool("raw", false, "include unexpected/unknown proto fields in the output")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: telemetry-decode [-json] [-raw] [file]\n\n"+
			"Decode the NGP telemetry store (settings-reset reports) into readable fields.\n"+
			"Reads the given JSONL file, or stdin when omitted or \"-\".\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	in := os.Stdin
	if args := flag.Args(); len(args) > 0 && args[0] != "-" {
		f, err := os.Open(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		in = f
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	var total, ok, bad int
	err := resetreport.ScanStore(in, func(rec resetreport.Record) {
		total++
		if rec.Err != nil {
			bad++
			if *jsonOut {
				_ = enc.Encode(map[string]any{
					"ts":          rec.TS,
					"time":        tsStr(rec.TS),
					"error":       rec.Err.Error(),
					"len":         rec.Len,
					"preview_hex": resetreport.HexPreview(rec.Raw, 64),
				})
			} else {
				fmt.Fprintf(out, "── record %d ─ %s ─ UNPARSEABLE (%v); %d bytes: %s\n\n",
					total, tsStr(rec.TS), rec.Err, rec.Len, resetreport.HexPreview(rec.Raw, 32))
			}
			return
		}
		ok++
		rep := rec.Report
		if !*showUnknown {
			rep.Unknown = nil
		}
		if *jsonOut {
			_ = enc.Encode(struct {
				TS     int64               `json:"ts"`
				Time   string              `json:"time"`
				Report *resetreport.Report `json:"report"`
			}{rec.TS, tsStr(rec.TS), rep})
		} else {
			writeText(out, total, rec.TS, rep)
		}
	})
	if err != nil {
		out.Flush()
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
		os.Exit(1)
	}
	out.Flush()
	fmt.Fprintf(os.Stderr, "\n%d record(s): %d decoded, %d unparseable\n", total, ok, bad)
}

func writeText(w io.Writer, idx int, ts int64, r *resetreport.Report) {
	s := resetreport.Safe
	fmt.Fprintf(w, "── record %d ─────────────────────────────── %s\n", idx, tsStr(ts))
	if r.ResetRequestOrigin != "" {
		fmt.Fprintf(w, "  reset origin  : %s\n", r.ResetRequestOrigin)
	}
	if r.StartupType != "" {
		fmt.Fprintf(w, "  startup type  : %s\n", r.StartupType)
	}
	for _, u := range r.StartupURLs {
		fmt.Fprintf(w, "  startup url   : %s\n", s(u))
	}
	if r.Homepage != "" || r.HomepageIsNTP != nil {
		extra := ""
		if r.HomepageIsNTP != nil {
			extra = fmt.Sprintf("   (new tab page: %v)", *r.HomepageIsNTP)
		}
		fmt.Fprintf(w, "  homepage      : %s%s\n", s(r.Homepage), extra)
	}
	if r.ShowHomeButton != nil {
		fmt.Fprintf(w, "  show home btn : %v\n", *r.ShowHomeButton)
	}
	if r.DefaultSearchEngine != "" {
		fmt.Fprintf(w, "  search engine : %s\n", s(r.DefaultSearchEngine))
	}
	if len(r.Extensions) > 0 {
		fmt.Fprintf(w, "  extensions    : %d\n", len(r.Extensions))
		for _, e := range r.Extensions {
			fmt.Fprintf(w, "      • %s  \"%s\"\n", s(e.ID), s(e.Name))
		}
	}
	if len(r.Shortcuts) > 0 {
		fmt.Fprintf(w, "  shortcuts     : %d\n", len(r.Shortcuts))
		for _, sc := range r.Shortcuts {
			fmt.Fprintf(w, "      • %s\n", s(sc))
		}
	}
	if r.GUID != "" {
		fmt.Fprintf(w, "  guid          : %s\n", s(r.GUID))
	}
	for _, u := range r.Unknown {
		fmt.Fprintf(w, "  [unknown]     : %s\n", u)
	}
	fmt.Fprintln(w)
}

func tsStr(ts int64) string {
	if ts <= 0 {
		return "(no timestamp)"
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

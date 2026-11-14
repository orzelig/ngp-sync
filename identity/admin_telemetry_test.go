package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tiny proto encoder, independent of the decoder, for building test bodies.
func uvarint(n uint64) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b = append(b, c|0x80)
		} else {
			return append(b, c)
		}
	}
}
func pTag(field, wt int) []byte { return uvarint(uint64(field)<<3 | uint64(wt)) }
func pStr(field int, v string) []byte {
	b := pTag(field, 2)
	b = append(b, uvarint(uint64(len(v)))...)
	return append(b, v...)
}
func pBytes(field int, v []byte) []byte {
	b := pTag(field, 2)
	b = append(b, uvarint(uint64(len(v)))...)
	return append(b, v...)
}
func pVar(field int, v uint64) []byte { return append(pTag(field, 0), uvarint(v)...) }

func writeStore(t *testing.T, bodies [][]byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	for _, body := range bodies {
		_ = enc.Encode(map[string]any{
			"ts":  int64(1700000000),
			"len": len(body),
			"b64": base64.StdEncoding.EncodeToString(body),
		})
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdminHTMLAndJSON(t *testing.T) {
	valid := append(pVar(19, 2), pStr(16, "https://duckduckgo.com/?q=%s")...)
	valid = append(valid, pStr(12, "https://example.org/")...)

	ext := append(pStr(1, "cjpalhdlnbpafiamejdnhcphjbkeiagm"), pStr(2, "\x1b]0;PWNED\x07evil")...)
	malicious := append(pVar(19, 1), pBytes(17, ext)...)

	junk := []byte{0xff, 0xfe, 0x01}

	ta := newTelemetryAdmin(writeStore(t, [][]byte{valid, malicious, junk}))

	// --- HTML ---
	rec := httptest.NewRecorder()
	ta.handleHTML(rec, httptest.NewRequest("GET", "/reports", nil))
	if rec.Code != 200 {
		t.Fatalf("HTML status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "duckduckgo.com") {
		t.Errorf("HTML missing decoded search engine")
	}
	if strings.ContainsRune(body, 0x1b) || strings.ContainsRune(body, 0x07) {
		t.Errorf("HTML contains a raw terminal escape byte (injection not neutralised)")
	}
	if !strings.Contains(body, `\x1b`) {
		t.Errorf("HTML should show the escape sequence escaped as \\x1b")
	}
	if strings.Contains(body, "<script") {
		t.Errorf("HTML should not contain an unescaped <script")
	}
	if !strings.Contains(body, "Unparseable") {
		t.Errorf("HTML should list the junk record under Unparseable")
	}

	// --- JSON (ndjson) ---
	rec = httptest.NewRecorder()
	ta.handleJSON(rec, httptest.NewRequest("GET", "/reports.json", nil))
	if rec.Code != 200 {
		t.Fatalf("JSON status = %d", rec.Code)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("JSON got %d lines, want 3", len(lines))
	}
	var first struct {
		Report struct {
			DefaultSearchEngine string `json:"default_search_engine"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("JSON line 0: %v", err)
	}
	if first.Report.DefaultSearchEngine != "https://duckduckgo.com/?q=%s" {
		t.Errorf("JSON decoded search engine = %q", first.Report.DefaultSearchEngine)
	}
	if strings.ContainsRune(rec.Body.String(), 0x1b) {
		t.Errorf("JSON contains a raw ESC byte")
	}
}

func TestAdminMissingFile(t *testing.T) {
	ta := newTelemetryAdmin(filepath.Join(t.TempDir(), "nope.jsonl"))
	rec := httptest.NewRecorder()
	ta.handleHTML(rec, httptest.NewRequest("GET", "/reports", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No reports yet") {
		t.Errorf("missing-file page should say 'No reports yet'")
	}

	rec = httptest.NewRecorder()
	ta.handleJSON(rec, httptest.NewRequest("GET", "/reports.json", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("missing-file JSON = %d %q, want 200 and empty", rec.Code, rec.Body.String())
	}
}

package resetreport

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// golden is a ChromeResetReport whose wire bytes were hand-computed from
// profile_reset_report.proto (independent of the decoder's own tables), so a
// wrong field number, wire type, or enum mapping is caught.
//
//	10 guid                       = "G1"
//	11 startup_type               = 3 (URLS)
//	12 startup_url_path           = "http://a/"
//	13 show_home_button           = true
//	14 homepage_is_new_tab_page   = false
//	15 homepage_path              = "http://hp/"
//	16 default_search_engine_path = "ddg"
//	17 enabled_extensions         = { id:"id1", name:"Name" }
//	18 shortcuts                  = "x --y"
//	19 reset_request_origin       = 2 (USER_CLICK)
const golden = "52024731" +
	"5803" +
	"6209687474703a2f2f612f" +
	"6801" +
	"7000" +
	"7a0a687474703a2f2f68702f" +
	"820103646467" +
	"8a010b0a0369643112044e616d65" +
	"92010578202d2d79" +
	"980102"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex: %v", err)
	}
	return b
}

func TestDecodeGolden(t *testing.T) {
	r, err := Decode(mustHex(t, golden))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.GUID != "G1" {
		t.Errorf("guid = %q, want G1", r.GUID)
	}
	if !strings.HasPrefix(r.StartupType, "URLS") {
		t.Errorf("startup_type = %q, want URLS…", r.StartupType)
	}
	if len(r.StartupURLs) != 1 || r.StartupURLs[0] != "http://a/" {
		t.Errorf("startup_urls = %q, want [http://a/]", r.StartupURLs)
	}
	if r.ShowHomeButton == nil || *r.ShowHomeButton != true {
		t.Errorf("show_home_button = %v, want true", r.ShowHomeButton)
	}
	if r.HomepageIsNTP == nil || *r.HomepageIsNTP != false {
		t.Errorf("homepage_is_new_tab_page = %v, want false", r.HomepageIsNTP)
	}
	if r.Homepage != "http://hp/" {
		t.Errorf("homepage = %q, want http://hp/", r.Homepage)
	}
	if r.DefaultSearchEngine != "ddg" {
		t.Errorf("default_search_engine = %q, want ddg", r.DefaultSearchEngine)
	}
	if len(r.Extensions) != 1 || r.Extensions[0].ID != "id1" || r.Extensions[0].Name != "Name" {
		t.Errorf("extensions = %+v, want [{id1 Name}]", r.Extensions)
	}
	if len(r.Shortcuts) != 1 || r.Shortcuts[0] != "x --y" {
		t.Errorf("shortcuts = %q, want [x --y]", r.Shortcuts)
	}
	if !strings.HasPrefix(r.ResetRequestOrigin, "USER_CLICK") {
		t.Errorf("reset_request_origin = %q, want USER_CLICK…", r.ResetRequestOrigin)
	}
	if len(r.Unknown) != 0 {
		t.Errorf("unknown = %v, want none", r.Unknown)
	}
}

func TestDecodeRepeated(t *testing.T) {
	wire := "620161" + "620162" + "8a01040a026531" + "8a01040a026532"
	r, err := Decode(mustHex(t, wire))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(r.StartupURLs) != 2 || r.StartupURLs[0] != "a" || r.StartupURLs[1] != "b" {
		t.Errorf("startup_urls = %q, want [a b]", r.StartupURLs)
	}
	if len(r.Extensions) != 2 || r.Extensions[0].ID != "e1" || r.Extensions[1].ID != "e2" {
		t.Errorf("extensions = %+v, want [{e1} {e2}]", r.Extensions)
	}
}

func TestDecodeUnknownField(t *testing.T) {
	wire := "52024731" + "980606" // field 10 "G1" + field 99 varint 6
	r, err := Decode(mustHex(t, wire))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.GUID != "G1" {
		t.Errorf("guid = %q, want G1", r.GUID)
	}
	if len(r.Unknown) != 1 || !strings.Contains(r.Unknown[0], "field 99") {
		t.Errorf("unknown = %v, want one entry naming field 99", r.Unknown)
	}
}

func TestDecodeMalformed(t *testing.T) {
	cases := map[string]string{
		"length overruns buffer": "6205",
		"truncated varint":       "08ff",
		"truncated submessage":   "8a0105ffff",
		"all ones":               "ffffffff",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mustHex(t, h)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestSafeStripsEscapes(t *testing.T) {
	in := "\x1b]0;pwned\x07evil\ttab"
	got := Safe(in)
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Errorf("Safe left a control byte in %q", got)
	}
	if !strings.Contains(got, "\\x1b") || !strings.Contains(got, "evil") {
		t.Errorf("Safe = %q, want escaped ESC and intact text", got)
	}
}

// ScanStore must decode good lines and surface bad ones without aborting.
func TestScanStore(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(mustHex(t, golden))
	junk := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x01})
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	_ = enc.Encode(lineRec{TS: 100, Len: 10, B64: good})
	_ = enc.Encode(lineRec{TS: 200, Len: 3, B64: junk})
	sb.WriteString("not json at all\n")

	var recs []Record
	if err := ScanStore(strings.NewReader(sb.String()), func(r Record) { recs = append(recs, r) }); err != nil {
		t.Fatalf("ScanStore: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	if recs[0].Err != nil || recs[0].Report == nil || recs[0].Report.GUID != "G1" {
		t.Errorf("record 0 = %+v, want decoded golden", recs[0])
	}
	if recs[1].Err == nil {
		t.Errorf("record 1 should be an unparseable protobuf")
	}
	if recs[2].Err == nil {
		t.Errorf("record 2 should be a bad JSON wrapper")
	}
}

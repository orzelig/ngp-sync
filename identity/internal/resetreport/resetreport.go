// Package resetreport decodes the serialized reset_report.ChromeResetReport
// protobuf that the NGP browser POSTs to id.ngp.one/t/reset (the opt-in
// settings-reset report), plus the JSONL store telemetry.go writes it into.
//
// It is dependency-free (no protoc) and treats every input as untrusted: the
// collection endpoint stores any body >= 2 bytes without validating it as a
// protobuf, so a stored line may be junk or crafted. Parsing is bounds-checked
// and never panics, and Safe neutralises terminal escape sequences in strings.
//
// Schema (profile_reset_report.proto, proto2):
//
//	10 guid                       string
//	11 startup_type               enum SessionStartupType
//	12 startup_url_path           repeated string
//	13 show_home_button           bool
//	14 homepage_is_new_tab_page   bool
//	15 homepage_path              string
//	16 default_search_engine_path string
//	17 enabled_extensions         repeated Extension { 1 id, 2 name }
//	18 shortcuts                  repeated string
//	19 reset_request_origin       enum ResetRequestOrigin
package resetreport

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Extension mirrors ChromeResetReport.Extension.
type Extension struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Report is a decoded ChromeResetReport. Pointer bools distinguish "absent"
// from "present and false" (proto2 optional semantics). String fields carry the
// human-readable enum label where applicable.
type Report struct {
	ResetRequestOrigin  string      `json:"reset_request_origin,omitempty"`
	StartupType         string      `json:"startup_type,omitempty"`
	StartupURLs         []string    `json:"startup_urls,omitempty"`
	Homepage            string      `json:"homepage,omitempty"`
	HomepageIsNTP       *bool       `json:"homepage_is_new_tab_page,omitempty"`
	ShowHomeButton      *bool       `json:"show_home_button,omitempty"`
	DefaultSearchEngine string      `json:"default_search_engine,omitempty"`
	Extensions          []Extension `json:"enabled_extensions,omitempty"`
	Shortcuts           []string    `json:"shortcuts,omitempty"`
	GUID                string      `json:"guid,omitempty"`
	Unknown             []string    `json:"unknown_fields,omitempty"`
}

// Record is one decoded line of the telemetry store.
type Record struct {
	TS     int64   // unix seconds the collector stamped
	Len    int     // raw protobuf length in bytes
	Raw    []byte  // raw protobuf bytes
	Report *Report // nil when Err != nil
	Err    error   // per-line decode failure (never fatal to the scan)
}

// maxLine bounds a single JSONL line: a 32 KiB body base64's to ~44 KiB plus
// the wrapper, so this leaves ample headroom while refusing a runaway line.
const maxLine = 8 * 1024 * 1024

type lineRec struct {
	TS  int64  `json:"ts"`
	Len int    `json:"len"`
	B64 string `json:"b64"`
}

// ScanStore reads JSONL telemetry lines from r, decoding each and invoking fn
// in order. Malformed lines are delivered with Record.Err set, never dropped
// and never fatal. The returned error is only a scanner-level read failure.
func ScanStore(r io.Reader, fn func(Record)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ln lineRec
		if err := json.Unmarshal(raw, &ln); err != nil {
			fn(Record{Err: fmt.Errorf("bad JSON wrapper: %w", err)})
			continue
		}
		body, err := base64.StdEncoding.DecodeString(ln.B64)
		if err != nil {
			fn(Record{TS: ln.TS, Len: ln.Len, Err: fmt.Errorf("bad base64: %w", err)})
			continue
		}
		rep, err := Decode(body)
		fn(Record{TS: ln.TS, Len: len(body), Raw: body, Report: rep, Err: err})
	}
	return sc.Err()
}

// Field numbers from profile_reset_report.proto.
const (
	fGUID         = 10
	fStartupType  = 11
	fStartupURL   = 12
	fShowHomeBtn  = 13
	fHomepageNTP  = 14
	fHomepagePath = 15
	fSearchEngine = 16
	fExtensions   = 17
	fShortcuts    = 18
	fResetOrigin  = 19
)

// Decode parses one serialized ChromeResetReport.
func Decode(b []byte) (*Report, error) {
	fields, err := parseFields(b)
	if err != nil {
		return nil, err
	}
	r := &Report{}
	for _, f := range fields {
		switch f.num {
		case fGUID:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			r.GUID = string(f.b)
		case fStartupType:
			if f.typ != wireVarint {
				r.addUnknown(f)
				continue
			}
			r.StartupType = startupType(f.u)
		case fStartupURL:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			r.StartupURLs = append(r.StartupURLs, string(f.b))
		case fShowHomeBtn:
			if f.typ != wireVarint {
				r.addUnknown(f)
				continue
			}
			v := f.u != 0
			r.ShowHomeButton = &v
		case fHomepageNTP:
			if f.typ != wireVarint {
				r.addUnknown(f)
				continue
			}
			v := f.u != 0
			r.HomepageIsNTP = &v
		case fHomepagePath:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			r.Homepage = string(f.b)
		case fSearchEngine:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			r.DefaultSearchEngine = string(f.b)
		case fExtensions:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			ext, err := decodeExtension(f.b)
			if err != nil {
				return nil, fmt.Errorf("enabled_extensions: %w", err)
			}
			r.Extensions = append(r.Extensions, ext)
		case fShortcuts:
			if f.typ != wireBytes {
				r.addUnknown(f)
				continue
			}
			r.Shortcuts = append(r.Shortcuts, string(f.b))
		case fResetOrigin:
			if f.typ != wireVarint {
				r.addUnknown(f)
				continue
			}
			r.ResetRequestOrigin = resetOrigin(f.u)
		default:
			r.addUnknown(f)
		}
	}
	return r, nil
}

func decodeExtension(b []byte) (Extension, error) {
	fields, err := parseFields(b)
	if err != nil {
		return Extension{}, err
	}
	var e Extension
	for _, f := range fields {
		if f.typ != wireBytes {
			continue
		}
		switch f.num {
		case 1:
			e.ID = string(f.b)
		case 2:
			e.Name = string(f.b)
		}
	}
	return e, nil
}

func startupType(v uint64) string {
	switch v {
	case 0:
		return "DEFAULT (new tab page)"
	case 1:
		return "HOMEPAGE (deprecated)"
	case 2:
		return "LAST (restore last session)"
	case 3:
		return "URLS (specific URLs)"
	case 4:
		return "LAST_AND_URLS"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", v)
	}
}

func resetOrigin(v uint64) string {
	switch v {
	case 0:
		return "UNSPECIFIED"
	case 1:
		return "UNKNOWN (direct navigation / command line)"
	case 2:
		return "USER_CLICK (reset button on settings page)"
	case 4:
		return "TRIGGERED_RESET (reset API)"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", v)
	}
}

func (r *Report) addUnknown(f wireField) {
	if f.typ == wireBytes {
		r.Unknown = append(r.Unknown,
			fmt.Sprintf("field %d (length-delimited, %d bytes): %s", f.num, len(f.b), HexPreview(f.b, 32)))
		return
	}
	r.Unknown = append(r.Unknown,
		fmt.Sprintf("field %d (wire type %d): %d", f.num, f.typ, f.u))
}

// ---- minimal protobuf wire reader --------------------------------------

const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireI32    = 5
)

type wireField struct {
	num int
	typ int
	u   uint64
	b   []byte
}

var errTruncated = errors.New("truncated input")

func parseFields(b []byte) ([]wireField, error) {
	var out []wireField
	i := 0
	for i < len(b) {
		tag, ni, err := readVarint(b, i)
		if err != nil {
			return nil, err
		}
		i = ni
		num := int(tag >> 3)
		typ := int(tag & 7)
		if num <= 0 {
			return nil, fmt.Errorf("invalid field number %d", num)
		}
		f := wireField{num: num, typ: typ}
		switch typ {
		case wireVarint:
			v, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			f.u, i = v, ni
		case wireI64:
			if i+8 > len(b) {
				return nil, errTruncated
			}
			f.u = le(b[i:i+8], 8)
			i += 8
		case wireBytes:
			l, ni, err := readVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = ni
			if l > uint64(len(b)-i) {
				return nil, errTruncated
			}
			f.b = b[i : i+int(l)]
			i += int(l)
		case wireI32:
			if i+4 > len(b) {
				return nil, errTruncated
			}
			f.u = le(b[i:i+4], 4)
			i += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d (field %d)", typ, num)
		}
		out = append(out, f)
	}
	return out, nil
}

func readVarint(b []byte, i int) (uint64, int, error) {
	var val uint64
	var shift uint
	for {
		if i >= len(b) {
			return 0, i, errTruncated
		}
		if shift >= 64 {
			return 0, i, errors.New("varint overflow")
		}
		c := b[i]
		i++
		val |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return val, i, nil
		}
		shift += 7
	}
}

func le(b []byte, n int) uint64 {
	var v uint64
	for k := 0; k < n; k++ {
		v |= uint64(b[k]) << (8 * uint(k))
	}
	return v
}

// ---- untrusted-string helpers ------------------------------------------

// Safe renders an untrusted string for a terminal: control bytes (including
// ESC, which could carry an ANSI injection), DEL, and the Unicode line and
// paragraph separators are escaped; tabs become spaces; everything else passes
// through. Run it on every stored string before display.
func Safe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r == 0xFEFF, r == 0x2028, r == 0x2029:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HexPreview renders up to n bytes of b as hex, eliding the rest.
func HexPreview(b []byte, n int) string {
	if len(b) > n {
		return fmt.Sprintf("%x…", b[:n])
	}
	return fmt.Sprintf("%x", b)
}

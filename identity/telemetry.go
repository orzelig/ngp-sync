package main

// Hardened, anonymous sink for NGP telemetry. Today it receives the opt-in
// settings-reset report the NGP browser POSTs when the user ticks "Help make
// NGP better by reporting the current settings" (the serialized
// ChromeResetReport protobuf) — see the browser's reset_report_uploader.cc,
// whose kResetReportUrl points here instead of Google.
//
// It is deliberately decoupled from the identity App: no auth, no access to the
// user store or the JWT secret. Because the endpoint is unauthenticated, abuse
// defense IS the design:
//   - per-request body cap (MaxBytesReader) — oversize is cut off, not buffered
//   - per-IP and global accept-rate limits (sliding window, bounded maps)
//   - a total on-disk budget, past which new reports are refused (no disk DoS)
//   - strict discard of anything malformed; nothing is ever echoed back
// Nothing identifying is persisted: the client posts cookieless, and the IP is
// used only in memory for rate limiting — never written to disk.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type telemetryCollector struct {
	path         string
	maxBody      int64 // bytes, per request
	totalCap     int64 // bytes, total on disk before refusing
	perIPHour    int   // accepted reports / IP / hour
	perMinAll    int   // accepted reports / minute across all IPs
	ipCapEntries int   // max distinct IPs tracked before the map is reset

	mu      sync.Mutex
	size    int64              // bytes currently on disk at path
	ipHits  map[string][]int64 // ip -> recent accepted unix secs (last hour)
	allHits []int64            // recent accepted unix secs across all IPs (last minute)
}

// telemetryPathFor derives the telemetry store path from the identity data file
// so it shares the same persistent volume (e.g. /data/telemetry.jsonl).
func telemetryPathFor(dataFile string) string {
	return filepath.Join(filepath.Dir(dataFile), "telemetry.jsonl")
}

func newTelemetryCollector(path string) *telemetryCollector {
	tc := &telemetryCollector{
		path:         path,
		maxBody:      32 << 10, // 32 KiB
		totalCap:     50 << 20, // 50 MiB
		perIPHour:    5,
		perMinAll:    60,
		ipCapEntries: 10000,
		ipHits:       map[string][]int64{},
	}
	if fi, err := os.Stat(path); err == nil {
		tc.size = fi.Size()
	}
	return tc
}

func (tc *telemetryCollector) handleResetReport(w http.ResponseWriter, r *http.Request) {
	// Cap the body before reading a single byte of it.
	r.Body = http.MaxBytesReader(w, r.Body, tc.maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBytesReader trips here when the client exceeds the cap.
		w.WriteHeader(http.StatusRequestEntityTooLarge) // 413
		return
	}
	if len(body) < 2 {
		// Empty / truncated — not a real report. Accept-and-drop, no storage.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ip := clientIP(r)
	now := time.Now().Unix()
	line := tc.encodeLine(now, body) // base64 work done outside the lock

	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Rate limits — a rejected request costs no quota (recordAcceptLocked only
	// runs on success), so a flood can't lock out a later legitimate report.
	if !tc.allowLocked(ip, now) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests) // 429
		return
	}
	// Total on-disk budget — protects the VM disk from unbounded growth.
	if tc.size+int64(len(line)) > tc.totalCap {
		w.WriteHeader(http.StatusInsufficientStorage) // 507
		return
	}
	if err := tc.appendLocked(line); err != nil {
		log.Printf("telemetry: append: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	tc.recordAcceptLocked(ip, now)
	w.WriteHeader(http.StatusNoContent) // 204 — never echo the input
}

func (tc *telemetryCollector) encodeLine(now int64, body []byte) []byte {
	rec := struct {
		TS  int64  `json:"ts"`
		Len int    `json:"len"`
		B64 string `json:"b64"`
	}{TS: now, Len: len(body), B64: base64.StdEncoding.EncodeToString(body)}
	b, _ := json.Marshal(&rec)
	return append(b, '\n')
}

func (tc *telemetryCollector) appendLocked(line []byte) error {
	f, err := os.OpenFile(tc.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(line)
	tc.size += int64(n)
	return err
}

// allowLocked reports whether a new report from ip is within both the per-IP
// hourly limit and the global per-minute limit. Call with tc.mu held; it does
// not record the hit.
func (tc *telemetryCollector) allowLocked(ip string, now int64) bool {
	tc.allHits = pruneSince(tc.allHits, now-60)
	if len(tc.allHits) >= tc.perMinAll {
		return false
	}
	hits := pruneSince(tc.ipHits[ip], now-3600)
	if len(hits) == 0 {
		delete(tc.ipHits, ip) // keep the map from accreting empty keys
	} else {
		tc.ipHits[ip] = hits
	}
	return len(hits) < tc.perIPHour
}

func (tc *telemetryCollector) recordAcceptLocked(ip string, now int64) {
	tc.allHits = append(tc.allHits, now)
	// Bound the IP map so many-source floods can't grow it without limit.
	// Legitimate traffic (settings resets) is tiny, so a wholesale reset is safe.
	if len(tc.ipHits) >= tc.ipCapEntries {
		tc.ipHits = map[string][]int64{}
	}
	tc.ipHits[ip] = append(tc.ipHits[ip], now)
}

// pruneSince drops leading entries <= cutoff from an ascending slice.
func pruneSince(xs []int64, cutoff int64) []int64 {
	i := 0
	for i < len(xs) && xs[i] <= cutoff {
		i++
	}
	return xs[i:]
}

// clientIP returns the client address for rate limiting. Behind Caddy the real
// client is the LAST entry Caddy appends to X-Forwarded-For (a client-supplied
// prefix is therefore untrusted and ignored). Falls back to RemoteAddr for
// direct/local connections.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

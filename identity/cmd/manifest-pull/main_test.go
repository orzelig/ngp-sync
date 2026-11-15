package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The tracked site/update/macos-arm64.json (ngp.29), signed by the release
// pipeline's real key: the fixed vector for the pinned format.
var ngp29 = manifest{
	Version: "156.0.8078.79-ngp.29",
	URL:     "https://github.com/orzelig/ngp-releases/releases/download/v156.0.8078.79-ngp.29/NGP-156.0.8078.79-ngp.29.dmg",
	SHA256:  "2378e51c35d7db8d1c7a547e6a06a7317d50fbacc8f6eb02b89967b17a1ee95f",
	Sig:     "FPv8vIdbhX8+BbaG5j6wxtlu5aH3NqE/jf9jrl9UFOZrtv3xjaZnlOP7RXy2s6o30k1uuncID2i+vThzVgIGCA==",
}

func pinnedKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	k, err := hex.DecodeString(pinnedPublicKeyHex)
	if err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatalf("pinned key: %v, len %d", err, len(k))
	}
	return k
}

func throwawayKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signed(priv ed25519.PrivateKey, ver string) manifest {
	m := manifest{
		Version: ver,
		URL:     "https://github.com/orzelig/ngp-releases/releases/download/v" + ver + "/NGP-" + ver + ".dmg",
		SHA256:  strings.Repeat("ab", 32),
	}
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedMessage(m)))
	return m
}

func TestFixedVectorVerifiesAgainstPinnedKey(t *testing.T) {
	if err := verify(pinnedKey(t), ngp29); err != nil {
		t.Fatalf("ngp.29 manifest does not verify against the pinned key: %v", err)
	}
}

func TestValidSignatureAccepted(t *testing.T) {
	pub, priv := throwawayKey(t)
	if err := verify(pub, signed(priv, "156.0.8078.80-ngp.30")); err != nil {
		t.Fatal(err)
	}
}

func TestTamperingRejected(t *testing.T) {
	pub, priv := throwawayKey(t)
	base := signed(priv, "156.0.8078.80-ngp.30")
	otherSig := signed(priv, "156.0.8078.80-ngp.31").Sig
	cases := map[string]func(*manifest){
		"version": func(m *manifest) { m.Version = "156.0.8078.80-ngp.31" },
		"sha256":  func(m *manifest) { m.SHA256 = strings.Repeat("cd", 32) },
		"url":     func(m *manifest) { m.URL = "https://evil.example/NGP.dmg" },
		"sig":     func(m *manifest) { m.Sig = otherSig },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := base
			mutate(&m)
			if err := verify(pub, m); err == nil {
				t.Fatal("tampered manifest verified")
			}
		})
	}
	// And the real vector, tampered, fails against the real key.
	m := ngp29
	m.Version = "156.0.8078.79-ngp.30"
	if verify(pinnedKey(t), m) == nil {
		t.Fatal("tampered ngp.29 verified against the pinned key")
	}
}

func TestSignatureEncodingRejected(t *testing.T) {
	pub, priv := throwawayKey(t)
	m := signed(priv, "156.0.8078.80-ngp.30")
	sig, _ := base64.StdEncoding.DecodeString(m.Sig)
	for name, s := range map[string]string{
		"non-base64": "not base64!!",
		"63 bytes":   base64.StdEncoding.EncodeToString(sig[:63]),
		"65 bytes":   base64.StdEncoding.EncodeToString(append(append([]byte{}, sig...), 0)),
		"url-safe":   base64.RawURLEncoding.EncodeToString(sig),
	} {
		t.Run(name, func(t *testing.T) {
			mm := m
			mm.Sig = s
			if verify(pub, mm) == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestVersionOrder(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"156.0.8078.79-ngp.9", "156.0.8078.79-ngp.10", -1},
		{"156.0.8078.79-ngp.29", "156.0.8078.80-ngp.1", -1},
		{"156.0.8078.79-ngp.29", "157.0.0.0-ngp.1", -1},
		{"156.0.8078.9-ngp.1", "156.0.8078.10-ngp.1", -1},
		{"156.0.8078.79-ngp.29", "156.0.8078.79-ngp.29", 0},
	}
	for _, c := range cases {
		a, err := parseVersion(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseVersion(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := compareVersions(a, b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := compareVersions(b, a); got != -c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	for _, bad := range []string{"", "156.0.8078.79", "156.0.8078-ngp.1", "v156.0.8078.79-ngp.1", "156.0.8078.79-ngp.1 ", "156.0.8078.79-ngp.4294967296", "156.0.8078.79-NGP.1"} {
		if _, err := parseVersion(bad); err == nil {
			t.Errorf("parseVersion(%q) accepted", bad)
		}
	}
}

// fakeGitHub serves /releases/latest and one asset, as api.github.com does.
type fakeGitHub struct {
	srv        *httptest.Server
	tag        string
	prerelease bool
	assetName  string
	asset      []byte
	latestCode int
	requests   []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{tag: "v156.0.8078.80-ngp.30", assetName: assetName, latestCode: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		switch r.URL.Path {
		case "/repos/" + releasesRepo + "/releases/latest":
			if f.latestCode != 200 {
				w.Header().Set("X-RateLimit-Reset", "1794730000")
				w.WriteHeader(f.latestCode)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name":   f.tag,
				"draft":      false,
				"prerelease": f.prerelease,
				"assets": []map[string]any{
					{"id": 11, "name": "NGP.dmg", "state": "uploaded"},
					{"id": 42, "name": f.assetName, "state": "uploaded"},
				},
			})
		case "/repos/" + releasesRepo + "/releases/assets/42":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("asset Accept = %q", r.Header.Get("Accept"))
			}
			w.Write(f.asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testConfig(f *fakeGitHub, pub ed25519.PublicKey, dir string) config {
	return config{apiBase: f.srv.URL, repo: releasesRepo, asset: assetName, pubKey: pub, dir: dir, client: f.srv.Client()}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// served writes the served manifest and returns its path and bytes.
func writeServed(t *testing.T, dir string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, assetName)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("served file changed:\n got %q\nwant %q", got, want)
	}
}

func assertOnlyServed(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != assetName {
			t.Errorf("unexpected entry left in dir: %s", e.Name())
		}
	}
}

func TestRunInstallsNewer(t *testing.T) {
	pub, priv := throwawayKey(t)
	f := newFakeGitHub(t)
	m := signed(priv, "156.0.8078.80-ngp.30")
	// Unknown fields in the asset are not carried over.
	raw := mustJSON(t, map[string]any{"version": m.Version, "url": m.URL, "sha256": m.SHA256, "sig": m.Sig, "extra": "<unsigned>"})
	f.asset = raw
	dir := t.TempDir()
	p := writeServed(t, dir, mustJSON(t, signed(priv, "156.0.8078.79-ngp.29")))

	msg, err := run(context.Background(), testConfig(f, pub, dir))
	if err != nil {
		t.Fatal(err)
	}
	if want := "installed 156.0.8078.80-ngp.30 (was 156.0.8078.79-ngp.29) from release v156.0.8078.80-ngp.30"; msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}
	want := fmt.Sprintf("{\n  \"version\": %q,\n  \"url\": %q,\n  \"sha256\": %q,\n  \"sig\": %q\n}\n", m.Version, m.URL, m.SHA256, m.Sig)
	assertBytes(t, p, []byte(want))
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", st.Mode().Perm())
	}
	assertOnlyServed(t, dir)
	// The written file verifies on its own.
	b, _ := os.ReadFile(p)
	got, err := parseManifest(b)
	if err != nil || verify(pub, got) != nil {
		t.Fatalf("written manifest does not verify: %v", err)
	}
}

func TestRunWritesTrackedShape(t *testing.T) {
	b, err := encodeManifest(ngp29)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := os.ReadFile("../../../site/update/macos-arm64.json")
	if err != nil {
		t.Skipf("tracked manifest not readable: %v", err)
	}
	if strings.TrimRight(string(b), "\n") != strings.TrimRight(string(tracked), "\n") {
		t.Fatalf("encoded shape differs from the tracked file:\n%s\nvs\n%s", b, tracked)
	}
}

func TestRunEqualAndOlderAreNoOps(t *testing.T) {
	pub, priv := throwawayKey(t)
	for _, c := range []struct {
		release, msg string
	}{
		{"156.0.8078.80-ngp.30", "current: serving 156.0.8078.80-ngp.30, release v156.0.8078.80-ngp.30 has the same"},
		{"156.0.8078.80-ngp.9", "kept 156.0.8078.80-ngp.30: release v156.0.8078.80-ngp.30 has older 156.0.8078.80-ngp.9"},
		{"156.0.8078.79-ngp.99", "kept 156.0.8078.80-ngp.30: release v156.0.8078.80-ngp.30 has older 156.0.8078.79-ngp.99"},
	} {
		t.Run(c.release, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.asset = mustJSON(t, signed(priv, c.release))
			dir := t.TempDir()
			// The served file's exact bytes, not re-encoded, so byte-identity is meaningful.
			before := append(mustJSON(t, signed(priv, "156.0.8078.80-ngp.30")), ' ', '\n')
			p := writeServed(t, dir, before)
			msg, err := run(context.Background(), testConfig(f, pub, dir))
			if err != nil {
				t.Fatal(err)
			}
			if msg != c.msg {
				t.Errorf("msg = %q, want %q", msg, c.msg)
			}
			assertBytes(t, p, before)
			assertOnlyServed(t, dir)
		})
	}
}

func TestRunRejectsAndKeepsServed(t *testing.T) {
	pub, priv := throwawayKey(t)
	good := signed(priv, "156.0.8078.80-ngp.30")
	tampered := good
	tampered.URL = "https://evil.example/NGP.dmg"
	field := func(drop string, empty bool) []byte {
		o := map[string]any{"version": good.Version, "url": good.URL, "sha256": good.SHA256, "sig": good.Sig}
		if empty {
			o[drop] = ""
		} else {
			delete(o, drop)
		}
		return mustJSON(t, o)
	}
	withSig := func(sig string) []byte { m := good; m.Sig = sig; return mustJSON(t, m) }
	rawSig, _ := base64.StdEncoding.DecodeString(good.Sig)
	badVersion := manifest{Version: "157-ngp.1", URL: good.URL, SHA256: good.SHA256}
	badVersion.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signedMessage(badVersion)))

	cases := map[string]func(*fakeGitHub){
		"tampered":      func(f *fakeGitHub) { f.asset = mustJSON(t, tampered) },
		"wrong key":     func(f *fakeGitHub) { _, other := throwawayKey(t); f.asset = mustJSON(t, signed(other, good.Version)) },
		"invalid JSON":  func(f *fakeGitHub) { f.asset = []byte(`{"version":`) },
		"JSON array":    func(f *fakeGitHub) { f.asset = []byte(`[` + string(mustJSON(t, good)) + `]`) },
		"JSON null":     func(f *fakeGitHub) { f.asset = []byte(`null`) },
		"trailing data": func(f *fakeGitHub) { f.asset = append(mustJSON(t, good), []byte(` {}`)...) },
		"non-string sig": func(f *fakeGitHub) {
			f.asset = []byte(strings.Replace(string(mustJSON(t, good)), `"`+good.Sig+`"`, `123`, 1))
		},
		"non-base64 sig": func(f *fakeGitHub) { f.asset = withSig("@@@@") },
		"63-byte sig":    func(f *fakeGitHub) { f.asset = withSig(base64.StdEncoding.EncodeToString(rawSig[:63])) },
		"65-byte sig": func(f *fakeGitHub) {
			f.asset = withSig(base64.StdEncoding.EncodeToString(append(append([]byte{}, rawSig...), 1)))
		},
		"bad version": func(f *fakeGitHub) { f.asset = mustJSON(t, badVersion) },
		"over 64 KiB": func(f *fakeGitHub) {
			o := map[string]any{"version": good.Version, "url": good.URL, "sha256": good.SHA256, "sig": good.Sig, "pad": strings.Repeat("x", 64<<10)}
			f.asset = mustJSON(t, o)
		},
		"no asset":   func(f *fakeGitHub) { f.assetName = "other.json" },
		"403":        func(f *fakeGitHub) { f.latestCode = http.StatusForbidden },
		"429":        func(f *fakeGitHub) { f.latestCode = http.StatusTooManyRequests },
		"prerelease": func(f *fakeGitHub) { f.prerelease = true; f.asset = mustJSON(t, good) },
	}
	for _, name := range []string{"version", "url", "sha256", "sig"} {
		name := name
		cases["missing "+name] = func(f *fakeGitHub) { f.asset = field(name, false) }
		cases["empty "+name] = func(f *fakeGitHub) { f.asset = field(name, true) }
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			setup(f)
			dir := t.TempDir()
			before := mustJSON(t, signed(priv, "156.0.8078.79-ngp.29"))
			p := writeServed(t, dir, before)
			if _, err := run(context.Background(), testConfig(f, pub, dir)); err == nil {
				t.Fatal("run succeeded")
			}
			assertBytes(t, p, before)
			assertOnlyServed(t, dir)
		})
	}
}

func TestRunNoAssetNamesTag(t *testing.T) {
	pub, _ := throwawayKey(t)
	f := newFakeGitHub(t)
	f.assetName = "other.json"
	_, err := run(context.Background(), testConfig(f, pub, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "release v156.0.8078.80-ngp.30 has no macos-arm64.json asset") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRateLimitIsNamed(t *testing.T) {
	pub, _ := throwawayKey(t)
	f := newFakeGitHub(t)
	f.latestCode = http.StatusForbidden
	_, err := run(context.Background(), testConfig(f, pub, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "X-RateLimit-Reset 1794730000") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunReplacesUnparsableServed(t *testing.T) {
	pub, priv := throwawayKey(t)
	for name, served := range map[string][]byte{
		"not JSON":    []byte("<html>oops"),
		"bad version": []byte(`{"version":"latest"}`),
		"no version":  []byte(`{}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			m := signed(priv, "156.0.8078.80-ngp.30")
			f.asset = mustJSON(t, m)
			dir := t.TempDir()
			p := writeServed(t, dir, served)
			msg, err := run(context.Background(), testConfig(f, pub, dir))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(msg, "(was none)") {
				t.Errorf("msg = %q", msg)
			}
			b, _ := os.ReadFile(p)
			got, err := parseManifest(b)
			if err != nil || got != m {
				t.Fatalf("served = %q, err %v", b, err)
			}
		})
	}
}

func TestRunCreatesMissingServed(t *testing.T) {
	pub, priv := throwawayKey(t)
	f := newFakeGitHub(t)
	m := signed(priv, "156.0.8078.80-ngp.30")
	f.asset = mustJSON(t, m)
	dir := t.TempDir()
	if _, err := run(context.Background(), testConfig(f, pub, dir)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, assetName))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseManifest(b); err != nil || got != m {
		t.Fatalf("served = %q, err %v", b, err)
	}
}

func TestRunFailedRenameLeavesNoPartialFile(t *testing.T) {
	pub, priv := throwawayKey(t)
	f := newFakeGitHub(t)
	f.asset = mustJSON(t, signed(priv, "156.0.8078.80-ngp.30"))
	dir := t.TempDir()
	before := mustJSON(t, signed(priv, "156.0.8078.79-ngp.29"))
	p := writeServed(t, dir, before)

	orig := rename
	rename = func(string, string) error { return errors.New("injected rename failure") }
	defer func() { rename = orig }()

	_, err := run(context.Background(), testConfig(f, pub, dir))
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("err = %v", err)
	}
	assertBytes(t, p, before)
	assertOnlyServed(t, dir)
}

func TestRunRemovesStaleTemps(t *testing.T) {
	pub, priv := throwawayKey(t)
	for name, setup := range map[string]func(*fakeGitHub){
		"installing": func(f *fakeGitHub) { f.asset = mustJSON(t, signed(priv, "156.0.8078.80-ngp.30")) },
		"failing":    func(f *fakeGitHub) { f.latestCode = http.StatusForbidden },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			setup(f)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".macos-arm64.json.tmp-x"), []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			run(context.Background(), testConfig(f, pub, dir))
			if _, err := os.Stat(filepath.Join(dir, ".macos-arm64.json.tmp-x")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale temp file still there: %v", err)
			}
		})
	}
}

// The key and the source cannot be changed at run time: main.go declares only
// the -dir flag and reads no environment variable.
func TestNoKeyOrSourceOption(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"os.Getenv", "os.LookupEnv", "os.Environ", "syscall.Getenv"} {
		if strings.Contains(s, banned) {
			t.Errorf("main.go uses %s", banned)
		}
	}
	flags := regexp.MustCompile(`flag\.(String|Bool|Int|Int64|Uint|Uint64|Float64|Duration|Var|Func|TextVar|BoolFunc)(Var)?\(`).FindAllString(s, -1)
	if len(flags) != 1 || !strings.Contains(s, `flag.String("dir"`) {
		t.Errorf("main.go declares flags %v, want only -dir", flags)
	}
	if !strings.Contains(s, `pinnedPublicKeyHex = "`+hex.EncodeToString(pinnedKey(t))+`"`) {
		t.Error("pinned key is not a constant in main.go")
	}
}

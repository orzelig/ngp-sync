// Command manifest-pull keeps ngp.one's update manifest current.
//
// It asks GitHub for the latest release of orzelig/ngp-releases, downloads its
// macos-arm64.json asset, verifies the ed25519 signature against the key pinned
// below, and installs it into the served directory only when its version is
// strictly newer than the one already there. GitHub is trusted for
// availability, never for content: what is written is the four verified
// fields, nothing else.
//
// It runs every 15 minutes from provision/systemd/ngp-manifest-pull.timer, in a
// one-shot container that mounts only the served directory. See
// docs/runbooks/manifest-pull.md.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// The source and the trust anchor are constants on purpose: no flag, no
// environment variable and no token can point the tool at another key or
// another repository.
const (
	pinnedPublicKeyHex = "cb0a6ad9c76683b4f3b4e0785870a7123772ccf1797b17532d199d492aa985b6"
	githubAPIBase      = "https://api.github.com"
	releasesRepo       = "orzelig/ngp-releases"
	assetName          = "macos-arm64.json"

	signatureDomain = "ngp-update-manifest-v1"
	tempPattern     = "." + assetName + ".tmp-*"
	userAgent       = "ngp-manifest-pull"

	maxReleaseBytes  = 1 << 20  // the release JSON
	maxManifestBytes = 64 << 10 // the manifest asset
	runTimeout       = 60 * time.Second
)

type config struct {
	apiBase string
	repo    string
	asset   string
	pubKey  ed25519.PublicKey
	dir     string
	client  *http.Client
}

// rename is os.Rename, a variable so a test can make the final step fail.
var rename = os.Rename

func main() {
	dir := flag.String("dir", "/update", "directory holding the served "+assetName)
	flag.Parse()

	log.SetFlags(0) // journald adds the timestamp
	log.SetPrefix("manifest-pull: ")

	key, err := hex.DecodeString(pinnedPublicKeyHex)
	if err != nil || len(key) != ed25519.PublicKeySize {
		log.Fatalf("failed: pinned public key is malformed")
	}
	cfg := config{
		apiBase: githubAPIBase,
		repo:    releasesRepo,
		asset:   assetName,
		pubKey:  ed25519.PublicKey(key),
		dir:     *dir,
		client:  &http.Client{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	msg, err := run(ctx, cfg)
	if err != nil {
		log.Printf("failed: %v", err)
		os.Exit(1)
	}
	log.Print(msg)
}

// manifest is the signed update manifest. Field order is the order it is
// written in, which is the tracked site/update/macos-arm64.json's shape.
type manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Sig     string `json:"sig"`
}

// run does one pull and returns the line to log on success.
func run(ctx context.Context, cfg config) (string, error) {
	removeStaleTemps(cfg.dir)

	served := filepath.Join(cfg.dir, cfg.asset)
	cur, err := servedVersion(served)
	if err != nil {
		return "", err
	}

	tag, raw, err := fetchLatest(ctx, cfg)
	if err != nil {
		return "", err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return "", fmt.Errorf("release %s: %w", tag, err)
	}
	if err := verify(cfg.pubKey, m); err != nil {
		return "", fmt.Errorf("release %s: %w", tag, err)
	}
	newV, err := parseVersion(m.Version)
	if err != nil {
		return "", fmt.Errorf("release %s: %w", tag, err)
	}

	if cur != nil {
		switch c := compareVersions(newV, *cur); {
		case c == 0:
			return fmt.Sprintf("current: serving %s, release %s has the same", cur, tag), nil
		case c < 0:
			return fmt.Sprintf("kept %s: release %s has older %s", cur, tag, newV), nil
		}
	}

	if err := install(cfg.dir, served, m); err != nil {
		return "", err
	}
	was := "none"
	if cur != nil {
		was = cur.String()
	}
	return fmt.Sprintf("installed %s (was %s) from release %s", newV, was, tag), nil
}

// removeStaleTemps deletes temp files a killed earlier run left behind.
// systemd never starts a one-shot unit that is still running, so none of them
// belongs to a live run.
func removeStaleTemps(dir string) {
	stale, _ := filepath.Glob(filepath.Join(dir, tempPattern))
	for _, p := range stale {
		_ = os.Remove(p)
	}
}

// servedVersion reads the version of the manifest being served. A missing or
// unparsable file has no version (nil) and will be replaced; any other read
// error is a failure, since not knowing what is served risks a downgrade.
func servedVersion(path string) (*version, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read served manifest: %w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &m) != nil {
		return nil, nil
	}
	v, err := parseVersion(m.Version)
	if err != nil {
		return nil, nil
	}
	return &v, nil
}

type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"assets"`
}

// fetchLatest returns the latest release's tag and its manifest asset's bytes.
// The asset is fetched by its numeric id, so no URL from a response is used.
func fetchLatest(ctx context.Context, cfg config) (string, []byte, error) {
	body, err := get(ctx, cfg, "/repos/"+cfg.repo+"/releases/latest", "application/vnd.github+json", maxReleaseBytes)
	if err != nil {
		return "", nil, fmt.Errorf("latest release: %w", err)
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", nil, fmt.Errorf("latest release: unparsable response: %v", err)
	}
	if rel.Draft || rel.Prerelease {
		return "", nil, fmt.Errorf("latest release %s is a draft or prerelease", rel.TagName)
	}
	var id int64
	for _, a := range rel.Assets {
		if a.Name == cfg.asset && a.State == "uploaded" {
			id = a.ID
			break
		}
	}
	if id == 0 {
		return "", nil, fmt.Errorf("release %s has no %s asset", rel.TagName, cfg.asset)
	}
	raw, err := get(ctx, cfg, "/repos/"+cfg.repo+"/releases/assets/"+strconv.FormatInt(id, 10), "application/octet-stream", maxManifestBytes)
	if err != nil {
		return "", nil, fmt.Errorf("release %s: asset %s: %w", rel.TagName, cfg.asset, err)
	}
	return rel.TagName, raw, nil
}

func get(ctx context.Context, cfg config, path, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	resp, err := cfg.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			msg += " (GitHub rate limit or access denied"
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				msg += ", Retry-After " + ra
			}
			if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				msg += ", X-RateLimit-Reset " + reset
			}
			msg += ")"
		}
		return nil, errors.New(msg)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response larger than %d bytes", limit)
	}
	return b, nil
}

// parseManifest requires exactly one JSON object whose version, url, sha256
// and sig are non-empty strings. Unknown fields are ignored (and never
// written back out).
func parseManifest(b []byte) (manifest, error) {
	if len(b) > maxManifestBytes {
		return manifest{}, fmt.Errorf("manifest larger than %d bytes", maxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return manifest{}, fmt.Errorf("manifest is not a JSON object: %v", err)
	}
	if obj == nil {
		return manifest{}, errors.New("manifest is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return manifest{}, errors.New("manifest has data after the JSON object")
	}
	var m manifest
	for _, f := range []struct {
		name string
		dst  *string
	}{{"version", &m.Version}, {"url", &m.URL}, {"sha256", &m.SHA256}, {"sig", &m.Sig}} {
		raw, ok := obj[f.name]
		if !ok {
			return manifest{}, fmt.Errorf("manifest has no %q", f.name)
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return manifest{}, fmt.Errorf("manifest %q is not a string", f.name)
		}
		if *f.dst == "" {
			return manifest{}, fmt.Errorf("manifest %q is empty", f.name)
		}
	}
	return m, nil
}

// signedMessage is what the release pipeline signs; it matches ngp-browser's
// tools/ngp-update/ngp-update-sign.py.
func signedMessage(m manifest) []byte {
	return []byte(signatureDomain + "\n" + m.Version + "\n" + m.SHA256 + "\n" + m.URL + "\n")
}

func verify(pub ed25519.PublicKey, m manifest) error {
	sig, err := base64.StdEncoding.DecodeString(m.Sig)
	if err != nil {
		return errors.New("signature is not standard base64")
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, signedMessage(m), sig) {
		return errors.New("bad signature")
	}
	return nil
}

// version is <a>.<b>.<c>.<d>-ngp.<n>: the Chromium version, then NGP's own
// build number.
type version struct {
	chromium [4]uint32
	ngp      uint32
}

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)\.(\d+)-ngp\.(\d+)$`)

func parseVersion(s string) (version, error) {
	g := versionRE.FindStringSubmatch(s)
	if g == nil {
		return version{}, fmt.Errorf("version %q is not <a>.<b>.<c>.<d>-ngp.<n>", s)
	}
	var n [5]uint32
	for i := range n {
		x, err := strconv.ParseUint(g[i+1], 10, 32)
		if err != nil {
			return version{}, fmt.Errorf("version %q has a part out of range", s)
		}
		n[i] = uint32(x)
	}
	return version{chromium: [4]uint32{n[0], n[1], n[2], n[3]}, ngp: n[4]}, nil
}

func (v version) String() string {
	c := v.chromium
	return fmt.Sprintf("%d.%d.%d.%d-ngp.%d", c[0], c[1], c[2], c[3], v.ngp)
}

// compareVersions orders by the Chromium parts numerically, then the ngp
// number: -1 if a < b, 0 if equal, 1 if a > b.
func compareVersions(a, b version) int {
	for i := range a.chromium {
		if a.chromium[i] != b.chromium[i] {
			if a.chromium[i] < b.chromium[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.ngp < b.ngp:
		return -1
	case a.ngp > b.ngp:
		return 1
	}
	return 0
}

// encodeManifest writes the four verified fields in the tracked file's shape.
func encodeManifest(m manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// install replaces the served manifest atomically: a temp file in the same
// directory, fsync, rename over it, fsync the directory. On any error the temp
// file is removed and the served file is left as it was.
func install(dir, served string, m manifest) (err error) {
	data, err := encodeManifest(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
			err = fmt.Errorf("install: %w", err)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(0o644); err != nil { // CreateTemp makes it 0600
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = rename(tmp, served); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

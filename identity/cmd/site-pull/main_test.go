package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every test builds a throwaway source repository with git in t.TempDir() and
// pulls from it. git is required: a missing git fails the tests rather than
// skipping them, since without it nothing here is checked.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

type source struct {
	t   *testing.T
	dir string
}

// newSource makes a repository whose main holds these files (path relative to
// the repository root, so site files are "site/...").
func newSource(t *testing.T, files map[string]string) *source {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for these tests: %v", err)
	}
	s := &source{t: t, dir: filepath.Join(t.TempDir(), "src")}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, s.dir, "init", "--quiet", "-b", "main")
	s.commit(files, nil)
	return s
}

// commit writes files, deletes the paths in remove, and commits on main.
func (s *source) commit(files map[string]string, remove []string) {
	s.t.Helper()
	for p, content := range files {
		full := filepath.Join(s.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	for _, p := range remove {
		gitT(s.t, s.dir, "rm", "-r", "--quiet", p)
	}
	gitT(s.t, s.dir, "add", "-A")
	gitT(s.t, s.dir, "commit", "--quiet", "--allow-empty", "-m", "change")
}

func (s *source) head() string { return gitT(s.t, s.dir, "rev-parse", "HEAD")[:12] }

type checkout struct {
	root string // stands for ~/ngp-sync
	site string // root/site
	cfg  config
}

func newCheckout(t *testing.T, s *source) *checkout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ngp-sync")
	c := &checkout{root: root, site: filepath.Join(root, "site")}
	if err := os.MkdirAll(c.site, 0o755); err != nil {
		t.Fatal(err)
	}
	c.cfg = config{url: s.dir, ref: "refs/heads/main", state: t.TempDir(), site: c.site}
	return c
}

func (c *checkout) run(t *testing.T) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return run(ctx, c.cfg)
}

func (c *checkout) mustRun(t *testing.T) string {
	t.Helper()
	msg, err := c.run(t)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return msg
}

func (c *checkout) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(c.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every path under dir (relative, slash-separated) to its kind,
// mode, content and mtime, without following symlinks.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case fi.IsDir():
			m[rel] = "dir " + fi.Mode().Perm().String()
		case fi.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			m[rel] = "link " + l
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			m[rel] = fi.Mode().Perm().String() + " " + fi.ModTime().UTC().Format(time.RFC3339Nano) + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func assertSame(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: %q -> %q", k, v, after[k])
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s appeared", k)
		}
	}
}

// files lists every non-directory under dir with its content.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for k, v := range snapshot(t, dir) {
		if strings.HasPrefix(v, "dir ") {
			continue
		}
		if strings.HasPrefix(v, "link ") {
			m[k] = v
			continue
		}
		parts := strings.SplitN(v, " ", 3)
		m[k] = parts[2]
	}
	return m
}

func assertFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := files(t, dir)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range keys {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected %s (all: %v)", k, keys)
		}
	}
}

// ageAll sets every file's mtime to the past, so a rewrite shows.
func ageAll(t *testing.T, dir string) {
	t.Helper()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
}

var baseSite = map[string]string{
	"site/index.html":              "<h1>NGP</h1>\n",
	"site/privacy.html":            "privacy\n",
	"site/css/a.css":               "body{}\n",
	"site/update/macos-arm64.json": "{\"from\":\"main\"}\n",
	"caddy/Caddyfile":              "ngp.one {}\n",
	"docker-compose.yml":           "services: {}\n",
}

func TestFirstRunCreatesCloneAndMatchesTree(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	msg := c.mustRun(t)
	if want := "updated to " + src.head() + ": 3 written, 0 removed"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if _, err := os.Stat(filepath.Join(c.cfg.state, "repo.git", "HEAD")); err != nil {
		t.Errorf("no clone in the state directory: %v", err)
	}
	assertFiles(t, c.site, map[string]string{
		"index.html":   "<h1>NGP</h1>\n",
		"privacy.html": "privacy\n",
		"css/a.css":    "body{}\n",
	})
	if fi, err := os.Stat(filepath.Join(c.site, "index.html")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("index.html mode: %v %v", fi, err)
	}
}

func TestChangesConvergeAndEmptiedDirectoryGoes(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	src.commit(map[string]string{
		"site/index.html":    "<h1>NGP 2</h1>\n",
		"site/img/new/x.svg": "<svg/>\n",
	}, []string{"site/privacy.html", "site/css"})
	msg := c.mustRun(t)
	if want := "updated to " + src.head() + ": 2 written, 2 removed"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	assertFiles(t, c.site, map[string]string{
		"index.html":    "<h1>NGP 2</h1>\n",
		"img/new/x.svg": "<svg/>\n",
	})
	if _, err := os.Lstat(filepath.Join(c.site, "css")); !os.IsNotExist(err) {
		t.Errorf("emptied css/ still there: %v", err)
	}
}

func TestSecondRunWritesNothing(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	ageAll(t, c.site)
	before := snapshot(t, c.site)
	msg := c.mustRun(t)
	if want := "current at " + src.head(); msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	assertSame(t, before, snapshot(t, c.site))
}

func TestRedeployedOlderSiteIsRepaired(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	// A redeploy by scp puts back older content and a stray file.
	c.write(t, "site/index.html", "old\n")
	c.write(t, "site/stale.html", "stale\n")
	msg := c.mustRun(t)
	if want := "updated to " + src.head() + ": 1 written, 1 removed"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	assertFiles(t, c.site, map[string]string{
		"index.html":   "<h1>NGP</h1>\n",
		"privacy.html": "privacy\n",
		"css/a.css":    "body{}\n",
	})
}

func TestSiteUpdateIsNeverTouched(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.write(t, "site/update/macos-arm64.json", "{\"served\":true}\n")
	c.write(t, "site/update/only-on-disk", "keep\n")
	ageAll(t, filepath.Join(c.site, "update"))
	upd, err := os.Stat(filepath.Join(c.site, "update"))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, filepath.Join(c.site, "update"))
	c.mustRun(t)
	src.commit(map[string]string{"site/update/macos-arm64.json": "{\"changed\":1}\n", "site/update/new.json": "{}\n"}, nil)
	c.mustRun(t)
	assertSame(t, before, snapshot(t, filepath.Join(c.site, "update")))
	upd2, err := os.Stat(filepath.Join(c.site, "update"))
	if err != nil || !os.SameFile(upd, upd2) {
		t.Errorf("site/update was replaced: %v", err)
	}
}

func TestSiteDirectoryKeepsItsInode(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	before, err := os.Stat(c.site)
	if err != nil {
		t.Fatal(err)
	}
	c.mustRun(t)
	src.commit(map[string]string{"site/index.html": "v2\n"}, nil)
	c.mustRun(t)
	after, err := os.Stat(c.site)
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("site/ was replaced: %v", err)
	}
}

func TestChangedFileIsReplacedByRename(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	p := filepath.Join(c.site, "index.html")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	src.commit(map[string]string{"site/index.html": "v2\n"}, nil)
	c.mustRun(t)
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("index.html was rewritten in place, not replaced by a rename")
	}
}

func TestModeOnlyChangeIsApplied(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	if err := os.Chmod(filepath.Join(src.dir, "site/privacy.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	src.commit(nil, nil)
	msg := c.mustRun(t)
	if want := "updated to " + src.head() + ": 1 written, 0 removed"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	fi, err := os.Stat(filepath.Join(c.site, "privacy.html"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("privacy.html mode = %v, %v; want 0755", fi.Mode().Perm(), err)
	}
}

// failsUnchanged runs once and requires a failure that changed nothing under
// the checkout.
func failsUnchanged(t *testing.T, c *checkout, wantInErr string) {
	t.Helper()
	ageAll(t, c.root)
	before := snapshot(t, c.root)
	msg, err := c.run(t)
	if err == nil {
		t.Fatalf("run succeeded: %q", msg)
	}
	if !strings.Contains(err.Error(), wantInErr) {
		t.Errorf("error %q does not mention %q", err, wantInErr)
	}
	assertSame(t, before, snapshot(t, c.root))
}

func TestFailedFetchLeavesSiteUnchanged(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	c.write(t, "site/local.html", "kept\n")
	c.cfg.url = filepath.Join(t.TempDir(), "does-not-exist")
	failsUnchanged(t, c, "fetch")
}

func TestTreeWithoutIndexIsRefused(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	src.commit(nil, []string{"site/index.html"})
	failsUnchanged(t, c, "index.html")

	// An empty or missing site/ is refused the same way.
	src.commit(nil, []string{"site"})
	failsUnchanged(t, c, "index.html")
}

func TestSymlinkInTreeIsRefused(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(src.dir, "site/link")); err != nil {
		t.Fatal(err)
	}
	src.commit(map[string]string{"site/index.html": "v2\n"}, nil)
	failsUnchanged(t, c, "not a regular file")
}

func TestSubmoduleInTreeIsRefused(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	head := gitT(t, src.dir, "rev-parse", "HEAD")
	gitT(t, src.dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",site/sub")
	gitT(t, src.dir, "commit", "--quiet", "-m", "submodule")
	failsUnchanged(t, c, "not a regular file")
}

func TestStaleTempFilesAreCleared(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	c.write(t, "site/.ngp-site-pull.123", "half")
	c.write(t, "site/css/.ngp-site-pull.456", "half")
	msg := c.mustRun(t)
	if want := "updated to " + src.head() + ": 0 written, 2 removed"; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	assertFiles(t, c.site, map[string]string{
		"index.html":   "<h1>NGP</h1>\n",
		"privacy.html": "privacy\n",
		"css/a.css":    "body{}\n",
	})
}

func TestNothingOutsideSiteIsTouched(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.write(t, "caddy/Caddyfile", "deployed\n")
	c.write(t, "docker-compose.yml", "deployed\n")
	c.write(t, "provision/vm-setup.sh", "deployed\n")
	ageAll(t, c.root)
	outside := func() map[string]string {
		m := snapshot(t, c.root)
		for k := range m {
			if k == "site" || strings.HasPrefix(k, "site/") {
				delete(m, k)
			}
		}
		return m
	}
	before := outside()
	c.mustRun(t)
	src.commit(map[string]string{"caddy/Caddyfile": "changed\n", "docker-compose.yml": "changed\n", "site/index.html": "v2\n"}, nil)
	c.mustRun(t)
	assertSame(t, before, outside())
}

func TestSymlinkInSiteIsReplacedNotFollowed(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	// site/css is a symlink to a directory outside the site, and
	// site/index.html a symlink to a file outside it.
	if err := os.Symlink(outside, filepath.Join(c.site, "css")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(c.site, "index.html")); err != nil {
		t.Fatal(err)
	}
	// Through the symlinked css/: a file already matching main's css/a.css
	// (must not count as current), and a directory where main has css/b.css
	// (must not be removed).
	if err := os.WriteFile(filepath.Join(outside, "a.css"), []byte("body{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "b.css", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	src.commit(map[string]string{"site/css/b.css": "b\n"}, nil)
	c.mustRun(t)
	assertFiles(t, c.site, map[string]string{
		"index.html":   "<h1>NGP</h1>\n",
		"privacy.html": "privacy\n",
		"css/a.css":    "body{}\n",
		"css/b.css":    "b\n",
	})
	assertFiles(t, outside, map[string]string{"target": "outside\n", "a.css": "body{}\n"})
	if _, err := os.Stat(filepath.Join(outside, "b.css", "keep")); err != nil {
		t.Errorf("a directory outside the site was removed: %v", err)
	}
}

func TestFileAndDirectorySwap(t *testing.T) {
	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.mustRun(t)
	// css/ becomes a file, and privacy.html becomes a directory.
	src.commit(nil, []string{"site/css", "site/privacy.html"})
	src.commit(map[string]string{"site/css": "now a file\n", "site/privacy.html/index.html": "now a dir\n"}, nil)
	c.mustRun(t)
	assertFiles(t, c.site, map[string]string{
		"index.html":              "<h1>NGP</h1>\n",
		"css":                     "now a file\n",
		"privacy.html/index.html": "now a dir\n",
	})
}

func TestSourceIsPinned(t *testing.T) {
	if sourceURL != "https://github.com/orzelig/ngp-sync.git" {
		t.Errorf("sourceURL = %q", sourceURL)
	}
	if sourceRef != "refs/heads/main" {
		t.Errorf("sourceRef = %q", sourceRef)
	}
}

// A repository that asks for a credential gets none: the run fails without
// prompting, and no request carries an Authorization header, even with an
// askpass program and credentials in the environment.
func TestNoCredentialIsEverSent(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		if a := r.Header.Get("Authorization"); a != "" {
			auth = append(auth, a)
		}
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	askpass := filepath.Join(t.TempDir(), "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho leaked-secret\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper'='!f() { echo username=u; echo password=leaked-secret; }; f'")

	src := newSource(t, baseSite)
	c := newCheckout(t, src)
	c.cfg.url = srv.URL + "/orzelig/ngp-sync.git"
	failsUnchanged(t, c, "fetch")
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatal("git never reached the server, so this proves nothing")
	}
	if len(auth) > 0 {
		t.Errorf("credentials were sent: %v", auth)
	}
}

func TestGitEnvironmentIsClosed(t *testing.T) {
	env := strings.Join(gitEnv("/state"), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("git environment lacks %s", want)
		}
	}
	if !strings.Contains(strings.Join(gitConfig, " "), "credential.helper=") {
		t.Error("git config does not clear credential.helper")
	}
}

func TestSafeRel(t *testing.T) {
	for _, ok := range []string{"index.html", "a/b.css", ".well-known/x"} {
		if !safeRel(ok) {
			t.Errorf("safeRel(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "/etc/x", "../x", "a/../x", "a//b", "./a", "a/.git/x", ".GIT", ".ngp-site-pull.1", `a\b`} {
		if safeRel(bad) {
			t.Errorf("safeRel(%q) = true", bad)
		}
	}
}

func TestBlobIDMatchesGit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	data := []byte("hello\x00world\n")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	want := gitT(t, dir, "hash-object", "--no-filters", p)
	if got := blobID(data, 40); got != want {
		t.Errorf("blobID = %s, git says %s", got, want)
	}
}

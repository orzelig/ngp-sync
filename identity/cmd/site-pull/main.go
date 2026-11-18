// Command site-pull keeps ngp.one's site/ current with orzelig/ngp-sync main.
//
// It fetches main into a bare, shallow clone that lives outside the checkout,
// then makes the checkout's site/ match main's site/ file by file: each file
// whose content or mode differs is written to a temp file in its directory and
// renamed into place, and files main no longer has are removed. site/ itself is
// never replaced, and nothing under site/update/ (the manifest puller's) or
// outside site/ is ever written. It decides by comparing each file's git blob
// id with main's tree, so it keeps no record of what it applied: a run at main
// writes nothing, and a run after a redeploy put back an older site/ repairs it.
//
// It runs every 15 minutes from provision/systemd/ngp-site-pull.timer, in a
// one-shot container that mounts only the clone's directory and site/. See
// docs/runbooks/site-pull.md and docs/adr/0002-site-pulled-from-main-into-the-checkout.md.
package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The source is constant on purpose: no flag, environment variable or token
// can point the tool at another repository or branch. The repository is
// public, so the fetch needs no credential, and git is run so that it can
// never prompt for, read or send one (gitEnv, gitConfig).
const (
	sourceURL = "https://github.com/orzelig/ngp-sync.git"
	sourceRef = "refs/heads/main"

	trackingRef = "refs/remotes/origin/main"
	treePrefix  = "site/"      // what is pulled
	skipDir     = "update"     // site/update/, the manifest puller's; never touched
	indexFile   = "index.html" // a tree without site/index.html is refused
	tempPrefix  = ".ngp-site-pull."
	repoDirName = "repo.git"

	runTimeout = 120 * time.Second // the unit's TimeoutStartSec is 180
)

type config struct {
	url   string // where to fetch from; sourceURL except in tests
	ref   string // the branch to fetch; sourceRef except in tests
	state string // holds the bare clone, repo.git
	site  string // the checkout's site/
}

func main() {
	state := flag.String("state", "/state", "directory holding the bare clone ("+repoDirName+")")
	site := flag.String("site", "/site", "the served site/ directory to keep current")
	flag.Parse()

	log.SetFlags(0) // journald adds the timestamp
	log.SetPrefix("site-pull: ")

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	msg, err := run(ctx, config{url: sourceURL, ref: sourceRef, state: *state, site: *site})
	if err != nil {
		log.Printf("failed: %v", err)
		os.Exit(1)
	}
	log.Print(msg)
}

// entry is one file of main's site/, by its path relative to site/.
type entry struct {
	rel  string
	mode os.FileMode // 0644 or 0755
	blob string      // the git object id
}

// run does one pull and returns the line to log on success. Nothing under the
// site directory is touched until the fetch has succeeded and the tree has
// passed every check.
func run(ctx context.Context, cfg config) (string, error) {
	if fi, err := os.Stat(cfg.site); err != nil {
		return "", fmt.Errorf("site directory: %w", err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("site directory: %s is not a directory", cfg.site)
	}

	repo := filepath.Join(cfg.state, repoDirName)
	if err := fetch(ctx, repo, cfg.url, cfg.ref); err != nil {
		return "", err
	}
	commit, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", trackingRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched main: %w", err)
	}
	sha := strings.TrimSpace(string(commit))
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}

	entries, err := readTree(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("main %s: %w", short, err)
	}

	written, err := writeChanged(ctx, repo, cfg.site, entries)
	if err != nil {
		return "", fmt.Errorf("main %s: %w (%d written before it)", short, err, written)
	}
	removed, err := removeExtra(cfg.site, entries)
	if err != nil {
		return "", fmt.Errorf("main %s: %w (%d written, %d removed before it)", short, err, written, removed)
	}
	if written == 0 && removed == 0 {
		return "current at " + short, nil
	}
	return fmt.Sprintf("updated to %s: %d written, %d removed", short, written, removed), nil
}

// gitConfig is passed to every git command: no credential helper and no
// askpass program, whatever the environment says.
var gitConfig = []string{"-c", "credential.helper=", "-c", "core.askPass="}

// gitEnv is the whole environment git runs with; nothing else is inherited.
// No global or system config is read, and git never prompts.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	}
}

// git runs one git command against the bare repository and returns its
// stdout. A failure carries git's last line of stderr.
func git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	full := append(append([]string{}, gitConfig...), "--git-dir="+repo)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv(filepath.Dir(repo))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %v%s", args[0], err, lastLine(errb.String()))
	}
	return out.Bytes(), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return ": " + strings.TrimSpace(s)
}

// fetch brings main into the bare clone, creating it on the first run.
func fetch(ctx context.Context, repo, url, ref string) error {
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := git(ctx, repo, "init", "--bare", "--quiet"); err != nil {
			return fmt.Errorf("create clone: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if _, err := git(ctx, repo, "fetch", "--quiet", "--depth", "1", "--no-tags", url, "+"+ref+":"+trackingRef); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

// readTree lists main's site/ without site/update/, and refuses a tree that
// must not be applied: one with no site/index.html, or with anything but a
// regular file (a symlink, a submodule) or an unsafe path under site/.
func readTree(ctx context.Context, repo string) ([]entry, error) {
	out, err := git(ctx, repo, "ls-tree", "-r", "-z", "--full-tree", trackingRef, "--", treePrefix)
	if err != nil {
		return nil, fmt.Errorf("read tree: %w", err)
	}
	var entries []entry
	hasIndex := false
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		if !ok {
			return nil, fmt.Errorf("unparsable tree entry %q", rec)
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			return nil, fmt.Errorf("unparsable tree entry %q", rec)
		}
		mode, typ, id := f[0], f[1], f[2]
		rel, ok := strings.CutPrefix(p, treePrefix)
		if !ok {
			return nil, fmt.Errorf("tree entry %q is outside %s", p, treePrefix)
		}
		if rel == skipDir || strings.HasPrefix(rel, skipDir+"/") {
			continue
		}
		var perm os.FileMode
		switch {
		case typ == "blob" && mode == "100644":
			perm = 0o644
		case typ == "blob" && mode == "100755":
			perm = 0o755
		default:
			return nil, fmt.Errorf("%s is not a regular file (mode %s, %s)", p, mode, typ)
		}
		if !safeRel(rel) {
			return nil, fmt.Errorf("unsafe path %q", p)
		}
		if !validID(id) {
			return nil, fmt.Errorf("%s has a malformed object id %q", p, id)
		}
		if rel == indexFile {
			hasIndex = true
		}
		entries = append(entries, entry{rel: rel, mode: perm, blob: id})
	}
	if !hasIndex {
		return nil, fmt.Errorf("tree has no %s%s; refusing to apply it", treePrefix, indexFile)
	}
	return entries, nil
}

// safeRel accepts a clean relative slash path with no empty, ".", ".." or
// ".git" component and no temp-file name, so it can only name a place under
// site/.
func safeRel(rel string) bool {
	if rel == "" || path.Clean(rel) != rel || path.IsAbs(rel) || strings.ContainsRune(rel, '\\') {
		return false
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") || strings.HasPrefix(c, tempPrefix) {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// blobID is git's object id for a blob with these bytes: SHA-1 for a 40-digit
// repository, SHA-256 for a 64-digit one.
func blobID(data []byte, idLen int) string {
	var h hash.Hash = sha1.New()
	if idLen == 64 {
		h = sha256.New()
	}
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// writeChanged makes each listed file's content and mode match the tree and
// returns how many it wrote or chmodded.
func writeChanged(ctx context.Context, repo, site string, entries []entry) (int, error) {
	n := 0
	for _, e := range entries {
		// Parents first: Lstat follows a symlink in a parent, so every parent
		// must be a real directory before dst is looked at, or a symlinked
		// directory could have a file outside the site read or removed.
		if err := ensureDir(site, path.Dir(e.rel)); err != nil {
			return n, err
		}
		dst := filepath.Join(site, filepath.FromSlash(e.rel))
		fi, err := os.Lstat(dst)
		switch {
		case err == nil && fi.Mode().IsRegular():
			data, err := os.ReadFile(dst)
			if err != nil {
				return n, fmt.Errorf("read %s: %w", e.rel, err)
			}
			if blobID(data, len(e.blob)) == e.blob {
				if fi.Mode().Perm() != e.mode {
					if err := os.Chmod(dst, e.mode); err != nil {
						return n, fmt.Errorf("chmod %s: %w", e.rel, err)
					}
					n++
				}
				continue
			}
		case err == nil && fi.IsDir():
			// main has a file where the site has a directory.
			if err := os.RemoveAll(dst); err != nil {
				return n, fmt.Errorf("replace directory %s: %w", e.rel, err)
			}
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return n, fmt.Errorf("stat %s: %w", e.rel, err)
		}
		if err := writeFile(ctx, repo, dst, e); err != nil {
			return n, fmt.Errorf("write %s: %w", e.rel, err)
		}
		n++
	}
	return n, nil
}

// ensureDir makes each component of rel (relative to site) a real directory,
// replacing a file or symlink in the way, so nothing is ever written through
// a symlink.
func ensureDir(site, rel string) error {
	if rel == "." {
		return nil
	}
	cur := site
	for _, c := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if err == nil && fi.IsDir() {
			continue
		}
		if err == nil {
			if err := os.Remove(cur); err != nil {
				return fmt.Errorf("replace %s with a directory: %w", cur, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", cur, err)
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", cur, err)
		}
	}
	return nil
}

// writeFile writes the blob to a temp file beside dst, sets its mode, syncs
// it and renames it over dst. On any error the temp file is removed and dst is
// left as it was.
func writeFile(ctx context.Context, repo, dst string, e entry) (err error) {
	f, err := os.CreateTemp(filepath.Dir(dst), tempPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	data, err := git(ctx, repo, "cat-file", "blob", e.blob)
	if err != nil {
		return err
	}
	if got := blobID(data, len(e.blob)); got != e.blob {
		return fmt.Errorf("blob %s read back as %s", e.blob, got)
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(e.mode); err != nil { // CreateTemp makes it 0600
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// removeExtra deletes everything under site that the tree does not list,
// outside site/update/, then each directory left empty. It never follows a
// symlink, and never removes site itself. Temp files a killed run left behind
// go the same way.
func removeExtra(site string, entries []entry) (int, error) {
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e.rel] = true
	}
	var dirs []string
	removed := 0
	err := filepath.WalkDir(site, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == site {
			return nil
		}
		rel, err := filepath.Rel(site, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == skipDir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if want[rel] {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove %s: %w", rel, err)
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, err
	}
	// Deepest first, so a directory whose only content was an empty
	// directory goes too.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			return removed, fmt.Errorf("read %s: %w", d, err)
		}
		if len(ents) > 0 {
			continue
		}
		if err := os.Remove(d); err != nil {
			return removed, fmt.Errorf("remove empty directory %s: %w", d, err)
		}
	}
	return removed, nil
}

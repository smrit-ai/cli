package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// hang is a hang limit only: no test asserts on how long anything took.
const hang = 60 * time.Second

const (
	oldBinary = "OLD BINARY\n"
	newBinary = "NEW BINARY\n"
	asset     = "smrit_linux_amd64.tar.gz"
)

// release is a fake GitHub: releases/latest redirects to the tag, and the
// download folder serves the tarball and SHA256SUMS. Every request is
// counted, with any Authorization header it carried.
type release struct {
	srv *httptest.Server

	mu        sync.Mutex
	hits      []string
	auths     []string
	tag       string
	tarball   []byte
	sums      string
	redirects map[string]string // path -> absolute URL to redirect to
	// chunked sends the tarball without a Content-Length.
	chunked bool
}

func tarball(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// In name order, so the same members always make the same bytes.
	for _, name := range slices.Sorted(maps.Keys(members)) {
		body := members[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sumLine(b []byte, name string) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]) + "  " + name + "\n"
}

func newRelease(t *testing.T) *release {
	t.Helper()
	r := &release{tag: "v1.2.3", redirects: map[string]string{}}
	r.tarball = tarball(t, map[string]string{"smrit": newBinary, "README.md": "not the binary\n"})
	r.sums = sumLine([]byte("other"), "smrit_darwin_arm64.tar.gz") + sumLine(r.tarball, asset)
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *release) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits = append(r.hits, req.URL.Path)
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	to, redirected := r.redirects[req.URL.Path]
	tag, tb, sums, chunked := r.tag, r.tarball, r.sums, r.chunked
	r.mu.Unlock()
	if redirected {
		http.Redirect(w, req, to, http.StatusFound)
		return
	}
	switch req.URL.Path {
	case "/releases/latest":
		http.Redirect(w, req, "/smrit-ai/cli/releases/tag/"+tag, http.StatusFound)
	case "/releases/latest/download/" + asset:
		if chunked {
			w.Write(tb[:1])
			w.(http.Flusher).Flush()
			tb = tb[1:]
		}
		w.Write(tb)
	case "/releases/latest/download/SHA256SUMS":
		w.Write([]byte(sums))
	default:
		http.NotFound(w, req)
	}
}

func (r *release) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

func (r *release) downloads() int {
	n := 0
	for _, p := range r.requests() {
		if strings.Contains(p, "/download/") {
			n++
		}
	}
	return n
}

// installed writes the old binary at rel under a fresh folder and returns
// its path.
func installed(t *testing.T, rel string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func upgrader(r *release, exe string) *Upgrader {
	return &Upgrader{
		Base:       r.srv.URL + "/releases/latest/download",
		Current:    "1.0.0",
		Executable: func() (string, error) { return exe, nil },
		HTTP:       r.srv.Client(),
		GOOS:       "linux",
		GOARCH:     "amd64",
	}
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), hang)
	t.Cleanup(cancel)
	return ctx
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// onlyBinary fails when the binary's folder holds anything but the binary.
func onlyBinary(t *testing.T, exe string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(exe) {
			t.Errorf("left in the binary's folder: %s", e.Name())
		}
	}
}

func refusedWith(t *testing.T, err error, words string) {
	t.Helper()
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, words) {
		t.Fatalf("want a refusal containing %q, got %v", words, err)
	}
	noDashes(t, re.Msg)
}

func noDashes(t *testing.T, s string) {
	t.Helper()
	if strings.ContainsAny(s, "—–") || strings.Contains(s, " - ") {
		t.Errorf("dash punctuation: %q", s)
	}
}

func TestUpgradeReplacesTheBinary(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "bin/smrit")
	res, err := upgrader(r, exe).Run(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded || res.Latest != "1.2.3" || res.Current != "1.0.0" || res.Path != exe {
		t.Fatalf("result %+v", res)
	}
	if read(t, exe) != newBinary {
		t.Fatal("the binary was not replaced")
	}
	st, err := os.Stat(exe)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	onlyBinary(t, exe)
	for _, a := range r.auths {
		if a != "" {
			t.Fatalf("a request carried Authorization: %q", a)
		}
	}
}

// A symbolic link to the binary is followed: the real file is replaced
// and the link still points at it.
func TestUpgradeFollowsALinkToTheBinary(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "real/smrit")
	link := filepath.Join(filepath.Dir(filepath.Dir(exe)), "link-smrit")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	res, err := upgrader(r, link).Run(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != exe || read(t, exe) != newBinary || read(t, link) != newBinary {
		t.Fatalf("result %+v", res)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
}

func TestUpgradeAlreadyLatestDownloadsNothing(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "bin/smrit")
	u := upgrader(r, exe)
	u.Current = "1.2.3"
	res, err := u.Run(testContext(t))
	if err != nil || res.Upgraded || res.Latest != "1.2.3" {
		t.Fatalf("%+v %v", res, err)
	}
	if r.downloads() != 0 || read(t, exe) != oldBinary {
		t.Fatalf("downloaded %v", r.requests())
	}
}

func TestCheckReportsWithoutDownloading(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "bin/smrit")
	res, err := upgrader(r, exe).Check(testContext(t))
	if err != nil || res.Latest != "1.2.3" || res.Current != "1.0.0" || res.UpToDate {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.requests(); len(got) != 1 || got[0] != "/releases/latest" {
		t.Fatalf("requests %v", got)
	}
	u := upgrader(r, exe)
	u.Current = "1.2.3"
	if res, err := u.Check(testContext(t)); err != nil || !res.UpToDate {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestCheckRefusesATagThatIsNotAVersion(t *testing.T) {
	r := newRelease(t)
	r.tag = "latest%0Aevil"
	_, err := upgrader(r, installed(t, "bin/smrit")).Check(testContext(t))
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("%v", err)
	}
}

// Each of these is refused, and the binary is left as it was.
func TestUpgradeRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *release, u *Upgrader)
		words string
	}{
		{"bad checksum", func(t *testing.T, r *release, u *Upgrader) {
			r.sums = strings.Repeat("0", 64) + "  " + asset + "\n"
		}, "checksum"},
		{"missing line", func(t *testing.T, r *release, u *Upgrader) {
			r.sums = sumLine(r.tarball, "other_"+asset)
		}, "exactly one"},
		{"two lines", func(t *testing.T, r *release, u *Upgrader) {
			r.sums = sumLine(r.tarball, asset) + sumLine(r.tarball, asset)
		}, "exactly one"},
		{"no smrit in the tarball", func(t *testing.T, r *release, u *Upgrader) {
			r.tarball = tarball(t, map[string]string{"other": newBinary})
			r.sums = sumLine(r.tarball, asset)
		}, "smrit"},
		{"tarball over the cap", func(t *testing.T, r *release, u *Upgrader) {
			u.MaxBytes = 16
		}, "larger"},
		{"tarball over the cap without a length", func(t *testing.T, r *release, u *Upgrader) {
			r.chunked = true
			u.MaxBytes = 16
		}, "larger"},
		{"redirect to another host", func(t *testing.T, r *release, u *Upgrader) {
			other := newRelease(t)
			r.redirects["/releases/latest/download/"+asset] = other.srv.URL + "/releases/latest/download/" + asset
			t.Cleanup(func() {
				if n := len(other.requests()); n != 0 {
					t.Errorf("the other host got %d requests", n)
				}
			})
		}, "redirected"},
		{"redirect to plain http", func(t *testing.T, r *release, u *Upgrader) {
			u.AllowedHosts = []string{hostOf(t, r.srv.URL)}
			r.redirects["/releases/latest/download/"+asset] = "http://" + hostOf(t, r.srv.URL) + "/x"
		}, "redirected"},
		{"a base that is not https", func(t *testing.T, r *release, u *Upgrader) {
			u.Base = "http" + strings.TrimPrefix(u.Base, "https")
		}, "https"},
		{"an unsupported platform", func(t *testing.T, r *release, u *Upgrader) {
			u.GOOS = "windows"
		}, "macOS and Linux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRelease(t)
			exe := installed(t, "bin/smrit")
			u := upgrader(r, exe)
			c.setup(t, r, u)
			_, err := u.Run(testContext(t))
			refusedWith(t, err, c.words)
			if read(t, exe) != oldBinary {
				t.Fatal("the binary changed")
			}
			onlyBinary(t, exe)
		})
	}
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// GitHub redirects a release download to its asset host; a redirect to an
// allowed host is followed.
func TestUpgradeFollowsARedirectToAnAllowedHost(t *testing.T) {
	r := newRelease(t)
	assets := newRelease(t)
	r.redirects["/releases/latest/download/"+asset] = assets.srv.URL + "/releases/latest/download/" + asset
	exe := installed(t, "bin/smrit")
	u := upgrader(r, exe)
	u.AllowedHosts = []string{hostOf(t, assets.srv.URL)}
	if _, err := u.Run(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if read(t, exe) != newBinary || assets.downloads() != 1 {
		t.Fatalf("binary %q, asset host requests %v", read(t, exe), assets.requests())
	}
	for _, a := range assets.auths {
		if a != "" {
			t.Fatalf("the asset host saw Authorization: %q", a)
		}
	}
}

// The real allow list is GitHub's own hosts and nothing else.
func TestDefaultAllowedHosts(t *testing.T) {
	want := []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}
	if fmt.Sprint(DefaultAllowedHosts) != fmt.Sprint(want) {
		t.Fatalf("%v", DefaultAllowedHosts)
	}
}

// A binary a package manager installed is upgraded by that package manager,
// and smrit says so before anything is downloaded.
func TestUpgradeRefusesAPackageManagersBinary(t *testing.T) {
	for _, rel := range []string{
		"opt/homebrew/bin/smrit",
		"usr/local/Cellar/smrit/1.0.0/bin/smrit",
		"nix/store/abc-smrit/bin/smrit",
		"usr/bin/smrit",
		"usr/sbin/smrit",
		"snap/smrit/1/bin/smrit",
	} {
		t.Run(rel, func(t *testing.T) {
			r := newRelease(t)
			exe := installed(t, rel)
			_, err := upgrader(r, exe).Run(testContext(t))
			refusedWith(t, err, "package manager")
			if read(t, exe) != oldBinary || len(r.requests()) != 0 {
				t.Fatalf("binary %q, requests %v", read(t, exe), r.requests())
			}
		})
	}
}

// A link into a package manager's tree is refused too, by where it leads.
func TestUpgradeRefusesALinkIntoAPackageManagersTree(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "opt/homebrew/Cellar/smrit/1.0.0/bin/smrit")
	link := filepath.Join(filepath.Dir(exe), "..", "..", "..", "..", "..", "smrit-link")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	_, err := upgrader(r, link).Run(testContext(t))
	refusedWith(t, err, "package manager")
}

func TestUpgradeRefusesAFolderItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read only folder")
	}
	r := newRelease(t)
	exe := installed(t, "ro/smrit")
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	_, err := upgrader(r, exe).Run(testContext(t))
	refusedWith(t, err, "cannot write")
	if read(t, exe) != oldBinary || r.downloads() != 0 {
		t.Fatalf("binary %q, requests %v", read(t, exe), r.requests())
	}
}

func TestUnavailableWhenTheReleaseCannotBeFetched(t *testing.T) {
	r := newRelease(t)
	exe := installed(t, "bin/smrit")
	u := upgrader(r, exe)
	u.Base = r.srv.URL + "/nowhere/download"
	_, err := u.Run(testContext(t))
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("%v", err)
	}
	noDashes(t, ue.Msg)
	if read(t, exe) != oldBinary {
		t.Fatal("the binary changed")
	}
	onlyBinary(t, exe)
}

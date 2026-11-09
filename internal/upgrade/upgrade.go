// Package upgrade replaces the running smrit binary with the latest release,
// as install.sh would install it: the tarball for this system and
// SHA256SUMS from the compiled in release address, exactly one checksum line
// for the tarball, the bytes verified, and only the smrit member written,
// beside the binary and renamed over it.
//
// The checksum guards against a corrupted or truncated download, not a
// compromised release, because SHA256SUMS comes from the same place.
//
// It never runs sudo and never touches a binary a package manager installed:
// that binary is the package manager's to replace.
package upgrade

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxTarball is the largest release tarball smrit downloads.
	MaxTarball = 64 << 20
	// maxSums bounds SHA256SUMS, and maxBinary the smrit member once
	// unpacked.
	maxSums   = 64 << 10
	maxBinary = 256 << 20
	// Timeout bounds one request, its body included.
	Timeout = 5 * time.Minute
)

// DefaultAllowedHosts are the hosts a release download may be redirected
// to: GitHub, and the hosts GitHub serves release assets from.
var DefaultAllowedHosts = []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// packageManaged are path fragments of folders a package manager owns.
var packageManaged = []string{"/Cellar/", "/homebrew/", "/nix/store/", "/usr/bin/", "/usr/sbin/", "/snap/"}

// versionTag is a release tag, as make release accepts it, with its v.
var versionTag = regexp.MustCompile(`^v([0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?)$`)

// versionParts splits a version as make release accepts it.
var versionParts = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-.]([0-9A-Za-z.]+))?$`)

// version is a parsed X.Y.Z with an optional suffix, such as rc.1 or dev.
type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	m := versionParts.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range v.n {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, false
		}
		v.n[i] = n
	}
	v.pre = m[4]
	return v, true
}

// compare orders versions by number, and a release after any version of
// the same number with a suffix.
func compare(a, b version) int {
	for i := range a.n {
		if c := cmp.Compare(a.n[i], b.n[i]); c != 0 {
			return c
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	return strings.Compare(a.pre, b.pre)
}

// judge sets whether this binary is the latest release, newer than it, or
// a development build, which has no place in the order.
func judge(st *Status) {
	cur, ok := parseVersion(st.Current)
	latest, lok := parseVersion(st.Latest)
	if !ok || !lok || strings.Contains(cur.pre, "dev") {
		st.Dev = true
		return
	}
	c := compare(cur, latest)
	st.UpToDate, st.Ahead = c == 0, c > 0
}

// RefusedError is an upgrade smrit will not do, with one sentence for a
// person. The binary is as it was.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// UnavailableError is a release that could not be fetched. The binary is as
// it was.
type UnavailableError struct{ Msg string }

func (e *UnavailableError) Error() string { return e.Msg }

func refused(format string, a ...any) error { return &RefusedError{fmt.Sprintf(format, a...)} }
func unavailable(format string, a ...any) error {
	return &UnavailableError{fmt.Sprintf(format, a...)}
}

// Upgrader upgrades one binary. Base, Current and Executable are required;
// every other zero value is the real behaviour.
type Upgrader struct {
	// Base is the latest release's download folder, compiled in; its
	// parent, releases/latest, redirects to the latest tag.
	Base string
	// Current is this binary's version.
	Current string
	// Executable finds the running binary, as os.Executable.
	Executable func() (string, error)
	// HTTP supplies the transport; its redirect policy and Authorization
	// are never used.
	HTTP *http.Client
	// AllowedHosts are the hosts a redirect may lead to; nil is
	// DefaultAllowedHosts.
	AllowedHosts []string
	// MaxBytes caps the tarball; 0 is MaxTarball.
	MaxBytes int64
	// GOOS and GOARCH name the tarball; empty is this system.
	GOOS, GOARCH string
}

// Status is what an upgrade or a check found.
type Status struct {
	Current  string `json:"current"`
	Latest   string `json:"latest"`
	UpToDate bool   `json:"up_to_date"`
	// Ahead is a binary newer than the latest release; Dev a development
	// build. Neither is replaced.
	Ahead bool `json:"ahead"`
	Dev   bool `json:"dev"`
	// Upgraded is whether the binary was replaced; Path is the binary.
	Upgraded bool   `json:"upgraded"`
	Path     string `json:"path,omitempty"`
}

// redirectError is a redirect smrit would not follow.
type redirectError struct{ to string }

func (e *redirectError) Error() string { return "redirected to " + e.to }

func (u *Upgrader) allowed(to *url.URL) bool {
	hosts := u.AllowedHosts
	if hosts == nil {
		hosts = DefaultAllowedHosts
	}
	if to.Scheme != "https" || to.User != nil {
		return false
	}
	for _, h := range hosts {
		if strings.EqualFold(to.Host, h) {
			return true
		}
	}
	return false
}

// client follows a redirect only over https to an allowed host, keeps no
// cookies and never adds a credential.
func (u *Upgrader) client(follow bool) *http.Client {
	hc := &http.Client{Timeout: Timeout}
	if u.HTTP != nil {
		hc.Transport = u.HTTP.Transport
	}
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) >= 5 || !u.allowed(req.URL) {
			return &redirectError{req.URL.Redacted()}
		}
		req.Header.Del("Authorization")
		return nil
	}
	return hc
}

// releases is the releases folder Base names: Base without its
// latest/download, so releases/latest and releases/download/<tag> hang off it.
func (u *Upgrader) releases() (string, error) {
	b, err := url.Parse(u.Base)
	if err != nil || b.Scheme != "https" || b.Host == "" || b.User != nil || b.RawQuery != "" || b.Fragment != "" ||
		!strings.HasSuffix(strings.TrimRight(b.Path, "/"), "/latest/download") {
		return "", refused("The release address this smrit was built with is not an https address, so it cannot upgrade itself.")
	}
	return strings.TrimSuffix(strings.TrimRight(u.Base, "/"), "/latest/download"), nil
}

func (u *Upgrader) current() string { return strings.TrimPrefix(u.Current, "v") }

// Check reports the latest release against this one, from where
// releases/latest redirects, and downloads nothing.
func (u *Upgrader) Check(ctx context.Context) (Status, error) {
	st := Status{Current: u.current()}
	releases, err := u.releases()
	if err != nil {
		return st, err
	}
	if st.Latest, err = u.latest(ctx, releases); err != nil {
		return st, err
	}
	judge(&st)
	return st, nil
}

// latest reads the tag releases/latest redirects to, without following it.
func (u *Upgrader) latest(ctx context.Context, releases string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releases+"/latest", nil)
	if err != nil {
		return "", unavailable("The latest release could not be looked up.")
	}
	resp, err := u.client(false).Do(req)
	if err != nil {
		return "", unavailable("The latest release could not be looked up, so try again.")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxSums))
	resp.Body.Close()
	loc, err := resp.Location()
	if resp.StatusCode < 300 || resp.StatusCode > 399 || err != nil {
		return "", unavailable("The release address did not name a latest release (HTTP %d).", resp.StatusCode)
	}
	dir, tag := path.Split(loc.Path)
	m := versionTag.FindStringSubmatch(tag)
	if path.Base(dir) != "tag" || m == nil {
		return "", unavailable("The latest release's tag is not a version this smrit can read.")
	}
	return m[1], nil
}

// get downloads target, refusing more than limit bytes.
func (u *Upgrader) get(ctx context.Context, target, name string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, unavailable("Could not download %s.", name)
	}
	resp, err := u.client(true).Do(req)
	if err != nil {
		var re *redirectError
		if errors.As(err, &re) {
			return nil, refused("The download of %s was redirected to %s, which is not a GitHub release host, so nothing was installed.", name, re.to)
		}
		return nil, unavailable("Could not download %s, so try again.", name)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, unavailable("Could not download %s (HTTP %d).", name, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, refused("%s is larger than %d bytes, so nothing was installed.", name, limit)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, unavailable("The download of %s was cut short, so try again.", name)
	}
	if int64(len(b)) > limit {
		return nil, refused("%s is larger than %d bytes, so nothing was installed.", name, limit)
	}
	return b, nil
}

// binary resolves the running binary through any link, and refuses one a
// package manager owns.
func (u *Upgrader) binary() (string, error) {
	exe, err := u.Executable()
	if err != nil {
		return "", refused("smrit could not find its own binary, so it cannot upgrade itself.")
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", refused("smrit could not find its own binary, so it cannot upgrade itself.")
	}
	for _, p := range []string{exe, real} {
		slashed := filepath.ToSlash(p)
		for _, frag := range packageManaged {
			if strings.Contains(slashed, frag) {
				return "", refused("smrit at %s was installed by a package manager, so upgrade it with that package manager.", p)
			}
		}
	}
	return real, nil
}

// Run upgrades the running binary to the latest release, unless it is that
// release already.
func (u *Upgrader) Run(ctx context.Context) (Status, error) {
	st := Status{Current: u.current()}
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if (goos != "darwin" && goos != "linux") || (goarch != "amd64" && goarch != "arm64") {
		return st, refused("smrit releases are built for macOS and Linux on 64 bit Intel and ARM only, so it cannot upgrade itself here.")
	}
	asset := "smrit_" + goos + "_" + goarch + ".tar.gz"
	releases, err := u.releases()
	if err != nil {
		return st, err
	}
	exe, err := u.binary()
	if err != nil {
		return st, err
	}
	st.Path = exe
	dir := filepath.Dir(exe)

	// The new binary is staged beside the old one, so the rename is atomic.
	// Creating it is also the check that the folder can be written.
	tmp, err := os.CreateTemp(dir, ".smrit-upgrade-*")
	if err != nil {
		return st, refused("smrit cannot write to %s, so it cannot upgrade itself there without sudo, which it never uses.", dir)
	}
	staged := tmp.Name()
	defer func() {
		if tmp != nil {
			tmp.Close()
		}
		if staged != "" {
			os.Remove(staged)
		}
	}()

	if st.Latest, err = u.latest(ctx, releases); err != nil {
		return st, err
	}
	if judge(&st); st.UpToDate || st.Ahead || st.Dev {
		return st, nil
	}
	// The assets of the tag just read, not of whatever is latest by the
	// time they are fetched, so the version reported is the one installed.
	// The tag passed versionTag, so it is safe in a path.
	base := releases + "/download/v" + st.Latest

	sums, err := u.get(ctx, base+"/SHA256SUMS", "SHA256SUMS", maxSums)
	if err != nil {
		return st, err
	}
	want, err := checksumFor(sums, asset)
	if err != nil {
		return st, err
	}
	limit := u.MaxBytes
	if limit <= 0 {
		limit = MaxTarball
	}
	tb, err := u.get(ctx, base+"/"+asset, asset, limit)
	if err != nil {
		return st, err
	}
	if got := sha256.Sum256(tb); hex.EncodeToString(got[:]) != want {
		return st, refused("The checksum of %s does not match SHA256SUMS, so nothing was installed.", asset)
	}
	if err := extract(tb, tmp); err != nil {
		return st, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return st, refused("smrit could not make the new binary executable in %s.", dir)
	}
	if err := tmp.Close(); err != nil {
		tmp = nil
		return st, refused("smrit could not write the new binary to %s.", dir)
	}
	tmp = nil
	if err := os.Rename(staged, exe); err != nil {
		return st, refused("smrit could not put the new binary in place at %s.", exe)
	}
	staged = ""
	st.Upgraded = true
	return st, nil
}

// checksumFor is the one SHA256SUMS line whose second field is asset.
func checksumFor(sums []byte, asset string) (string, error) {
	var found []string
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == asset {
			found = append(found, f[0])
		}
	}
	if len(found) != 1 {
		return "", refused("SHA256SUMS must have exactly one entry for %s, so nothing was installed.", asset)
	}
	h := strings.ToLower(found[0])
	if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
		return "", refused("SHA256SUMS has no usable checksum for %s, so nothing was installed.", asset)
	}
	return h, nil
}

// extract writes the tarball's smrit member, a regular file, to out, and
// nothing else.
func extract(tb []byte, out *os.File) error {
	bad := refused("The release tarball has no smrit binary in it, so nothing was installed.")
	gz, err := gzip.NewReader(bytes.NewReader(tb))
	if err != nil {
		return bad
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return bad
		}
		if path.Clean(h.Name) != "smrit" {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxBinary {
			return bad
		}
		n, err := io.Copy(out, io.LimitReader(tr, h.Size))
		if err != nil || n != h.Size {
			return refused("The new binary could not be written beside the old one, so nothing was installed.")
		}
		return nil
	}
}

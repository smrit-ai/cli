package retire

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/smrit-ai/cli/internal/scan"
)

// gitHeads opens each repository holding a clean file once, through the
// hardened runner, and asks it what HEAD holds.
type gitHeads struct {
	ctx   context.Context
	opts  scan.Options
	repos map[string]*headRepo
}

type headRepo struct {
	repo   *scan.Repo
	format string
	err    error
}

func (h *gitHeads) open(top string) (*headRepo, error) {
	if r, ok := h.repos[top]; ok {
		return r, r.err
	}
	r := &headRepo{}
	h.repos[top] = r
	repo, _ := scan.OpenRepo(h.ctx, top, h.opts)
	switch {
	case repo == nil:
		r.err = errors.New("the repository could not be opened")
	case repo.Top != top:
		r.err = errors.New("the repository moved")
	default:
		r.repo = repo
		r.format, r.err = repo.ObjectFormat(h.ctx)
	}
	return r, r.err
}

// head is the blob HEAD holds at rel in the repository at top, and the
// repository's object format. It asks git each time.
func (h *gitHeads) head(top, rel string) (format, blob string, err error) {
	r, err := h.open(top)
	if err != nil {
		return "", "", err
	}
	blob, err = r.repo.HeadBlob(h.ctx, rel)
	return r.format, blob, err
}

// hashFile reads rel once and returns the SHA-256 of its bytes and, when
// format is sha1 or sha256, the id git gives those bytes as a blob.
func hashFile(root *os.Root, rel, format string) (sha, blob string, err error) {
	f, info, err := openRegular(root, rel)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	var b hash.Hash
	switch format {
	case "sha1":
		b = sha1.New()
	case "sha256":
		b = sha256.New()
	}
	w := io.Writer(h)
	if b != nil {
		fmt.Fprintf(b, "blob %d\x00", info.Size())
		w = io.MultiWriter(h, b)
	}
	n, err := io.Copy(w, f)
	if err != nil {
		return "", "", err
	}
	sha = hex.EncodeToString(h.Sum(nil))
	// A file that grew or shrank while read has no blob id for its header.
	if b != nil && n == info.Size() {
		blob = hex.EncodeToString(b.Sum(nil))
	}
	return sha, blob, nil
}

// relTo is p, a path in the folder at rootPath, relative to top with
// forward slashes.
func relTo(top, rootPath, p string) (string, bool) {
	rel, err := filepath.Rel(top, filepath.Join(rootPath, filepath.FromSlash(p)))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// confirmClean keeps for deletion only the clean files whose bytes are the
// blob HEAD holds at their path. The rest move to the zip, except a file
// that no longer reads as imported, which is left.
func confirmClean(root *os.Root, rootPath string, heads *gitHeads, items []item, rep *Report) (keep, zip []item) {
	for _, it := range items {
		rel, ok := relTo(it.top, rootPath, it.path)
		var format, head string
		var err error
		if ok {
			format, head, err = heads.head(it.top, rel)
		}
		sha, blob, rerr := hashFile(root, it.path, format)
		switch {
		case rerr != nil || sha != it.sha:
			rep.Left = append(rep.Left, Left{it.path, reasonDuring})
		case !ok || err != nil || head == "" || blob != head:
			it.unpushed = false
			zip = append(zip, it)
		default:
			it.repoRel, it.format = rel, format
			keep = append(keep, it)
		}
	}
	return keep, zip
}

// remover removes one file at a time. The file is first moved to a fresh
// name in its own folder, hashed there, and unlinked only when the moved
// bytes are the ones checked: an edit landing after the last check is
// either in the moved file, which is then put back, or in a new file at the
// old name, which is never touched.
type remover struct {
	root   *os.Root
	rename Renamer
	link   func(root *os.Root, oldname, newname string) error
	heads  *gitHeads
}

// remove returns removed true when the file is gone. Otherwise left says
// why it is still in place, and stop whether retire must stop: the file
// could not be removed at all.
func (rm *remover) remove(it item) (removed bool, left string, stop bool) {
	const cannot = "it could not be removed"
	tmp, err := rm.placeholder(it.path)
	if err != nil {
		return false, cannot, true
	}
	if err := rm.rename.Rename(rm.root, filepath.FromSlash(it.path), filepath.FromSlash(tmp)); err != nil {
		rm.root.Remove(filepath.FromSlash(tmp))
		return false, cannot, true
	}
	sha, blob, err := hashFile(rm.root, tmp, it.format)
	same := err == nil && sha == it.sha
	if same && it.repoRel != "" {
		// A file deleted because git holds it: HEAD still holds these
		// exact bytes.
		_, head, herr := rm.heads.head(it.top, it.repoRel)
		same = herr == nil && head != "" && blob == head
	}
	if !same {
		return false, rm.putBack(tmp, it.path, reasonDuring), false
	}
	if err := rm.root.Remove(filepath.FromSlash(tmp)); err != nil {
		return false, rm.putBack(tmp, it.path, cannot), true
	}
	return true, "", false
}

// placeholder makes an empty file with a fresh name beside rel, created
// exclusively, for the rename to replace.
func (rm *remover) placeholder(rel string) (string, error) {
	dir := path.Dir(rel)
	for range 5 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		name := scan.RetireTempPrefix + hex.EncodeToString(b) + scan.RetireTempSuffix
		if dir != "." {
			name = dir + "/" + name
		}
		f, err := rm.root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		f.Close()
		return name, nil
	}
	return "", errors.New("could not find a free temporary name")
}

// putBack returns the moved file to its name, never over a file that took
// the name meanwhile. It returns reason, why the file is still here, when
// the file is back, and the same reason with where it is kept when it could
// not be put back.
//
// Only a hard link puts it back, because a link fails when the name is
// taken. A rename would replace a file that took the name between a check
// and the rename, so in a folder that takes no hard links the file stays
// under its temporary name, where scan and retire list it as a leftover.
func (rm *remover) putBack(tmp, rel, reason string) string {
	from, to := filepath.FromSlash(tmp), filepath.FromSlash(rel)
	err := rm.link(rm.root, from, to)
	if err == nil {
		rm.root.Remove(from)
		return reason
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Sprintf("%s and is kept as %s, because another file now has its name", reason, tmp)
	}
	return fmt.Sprintf("%s and is kept as %s, because this folder takes no hard link to put it back without risk of replacing another file", reason, tmp)
}

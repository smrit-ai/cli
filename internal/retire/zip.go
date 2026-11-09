package retire

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// ZipFile is the zip as it is written.
type ZipFile interface {
	io.Writer
	Sync() error
	Close() error
}

// ZipReader is the zip reopened for the read back.
type ZipReader interface {
	io.ReaderAt
	Close() error
}

// ZipStore creates the zip and reopens it, inside the folder. The real one
// is the disk; a test wraps it to make one step fail.
type ZipStore interface {
	// Create makes a new file name, failing with fs.ErrExist when one is
	// there, readable by its owner only.
	Create(root *os.Root, name string) (ZipFile, error)
	// Open reopens name for reading and returns its size.
	Open(root *os.Root, name string) (ZipReader, int64, error)
}

type disk struct{}

func (disk) Create(root *os.Root, name string) (ZipFile, error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	// The open made it 0600; a umask can only narrow that. Check anyway.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		f.Close()
		root.Remove(name)
		return nil, errors.New("the zip could not be made private")
	}
	return f, nil
}

func (disk) Open(root *os.Root, name string) (ZipReader, int64, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, errors.New("the zip is not a regular file")
	}
	return f, st.Size(), nil
}

// maxSuffix is how many zips one folder may hold for one day.
const maxSuffix = 999

// zipNameFor is the zip's name for day, with the suffix n when n > 1.
func zipNameFor(day time.Time, n int) string {
	name := "smrit-import-" + day.Format("2006-01-02")
	if n > 1 {
		name += fmt.Sprintf("-%d", n)
	}
	return name + ".zip"
}

// writeZip creates the first free zip name for day and writes every item
// into it, reading each file once and hashing exactly the bytes it stores.
// It returns the name it created; on an error the caller removes it.
func writeZip(root *os.Root, zs ZipStore, day time.Time, taken func(name string) (bool, error), items []item) (name string, err error) {
	var f ZipFile
	for n := 1; ; n++ {
		if n > maxSuffix {
			return "", errors.New("the folder already holds too many zips from today")
		}
		name = zipNameFor(day, n)
		t, err := taken(name)
		if err != nil {
			return "", err
		}
		if t {
			continue
		}
		f, err = zs.Create(root, name)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	zw := zip.NewWriter(f)
	for _, it := range items {
		if err := addMember(zw, root, it); err != nil {
			return name, err
		}
	}
	if err := zw.Close(); err != nil {
		return name, err
	}
	if err := f.Sync(); err != nil {
		return name, err
	}
	closed = true
	if err := f.Close(); err != nil {
		return name, err
	}
	// The folder's entry for the zip is made durable too, where the
	// system allows it.
	if d, err := root.Open("."); err == nil {
		d.Sync()
		d.Close()
	}
	return name, nil
}

func addMember(zw *zip.Writer, root *os.Root, it item) error {
	src, info, err := openRegular(root, it.path)
	if err != nil {
		return fmt.Errorf("%s could not be read: %w", it.path, err)
	}
	defer src.Close()
	hdr := &zip.FileHeader{Name: it.path, Method: zip.Deflate, Modified: info.ModTime()}
	hdr.SetMode(info.Mode().Perm())
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), src); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != it.sha {
		return fmt.Errorf("%s changed while it was being zipped", it.path)
	}
	return nil
}

// verifyZip reopens the zip and checks that it holds exactly the items,
// each hashing to the bytes the import sent.
func verifyZip(root *os.Root, zs ZipStore, name string, items []item) error {
	r, size, err := zs.Open(root, name)
	if err != nil {
		return err
	}
	defer r.Close()
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	want := map[string]string{}
	for _, it := range items {
		want[it.path] = it.sha
	}
	seen := map[string]bool{}
	for _, m := range zr.File {
		sha, ok := want[m.Name]
		if !ok || seen[m.Name] {
			return fmt.Errorf("the zip holds an unexpected member")
		}
		seen[m.Name] = true
		rc, err := m.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != sha {
			return fmt.Errorf("the zip's copy of %s does not match", m.Name)
		}
	}
	if len(seen) != len(want) {
		return errors.New("the zip is missing a file")
	}
	return nil
}

// firstFree is the first zip name for day that is not taken. It reads only.
func firstFree(day time.Time, taken func(name string) (bool, error)) (string, error) {
	for n := 1; n <= maxSuffix; n++ {
		name := zipNameFor(day, n)
		t, err := taken(name)
		if err != nil {
			return "", err
		}
		if !t {
			return name, nil
		}
	}
	return "", errors.New("the folder already holds too many zips from today")
}

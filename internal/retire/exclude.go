package retire

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/smrit-ai/cli/internal/scan"
)

// maxExcludeBytes bounds how much of an exclude file is read.
const maxExcludeBytes = 1 << 20

// excludeLine is the pattern matching exactly the repository relative path
// rel: anchored at the top, with git's wildcard characters escaped. It is
// "" for a path no single line can name.
func excludeLine(rel string) string {
	if strings.ContainsAny(rel, "\n\r") || rel == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('/')
	for _, r := range rel {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// excludePlan is what adding the zip to info/exclude takes: the file and
// the line, or nothing when git already ignores the zip, the line is there,
// or no line can name it.
type excludePlan struct {
	file, line string
}

// planExclude decides whether the zip at rel, relative to repo's top, needs
// a line in the repository's info/exclude. It only reads.
func planExclude(ctx context.Context, repo *scan.Repo, rel string) (*excludePlan, error) {
	if ignored, err := repo.Ignored(ctx, rel); err == nil && ignored {
		return nil, nil
	}
	line := excludeLine(rel)
	if line == "" {
		return nil, nil
	}
	p, err := repo.GitPath(ctx, "info/exclude")
	if err != nil {
		return nil, err
	}
	existing, err := readExclude(p)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimRight(l, "\r") == line {
			return nil, nil
		}
	}
	return &excludePlan{file: p, line: line}, nil
}

// readExclude reads the exclude file, never through a symbolic link; a file
// that is not there reads as empty.
func readExclude(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("the exclude file is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxExcludeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxExcludeBytes {
		return nil, errors.New("the exclude file is too large to read")
	}
	return b, nil
}

// apply appends the line, creating the info folder and the file when they
// are missing. It never follows a symbolic link and never touches a file
// git tracks.
func (e *excludePlan) apply() error {
	dir := filepath.Dir(e.file)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return errors.New("the repository's info folder is not a folder")
	}
	f, err := os.OpenFile(e.file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return errors.New("the exclude file is not a regular file")
	}
	existing, err := readExclude(e.file)
	if err != nil {
		f.Close()
		return err
	}
	data := e.line + "\n"
	if n := len(existing); n > 0 && existing[n-1] != '\n' {
		data = "\n" + data
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

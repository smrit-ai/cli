// Package retire removes the local files an import run moved into Smrit,
// once Smrit holds them: a file git can restore is deleted from the working
// tree, and any other is put into a zip in the folder first. It decides
// everything again from a fresh scan, never from what the run remembers
// alone, and removes nothing until the zip has been read back.
//
// This code deletes a person's files. Every rule here leaves a file in
// place when in doubt.
package retire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
)

// Options adjusts a retire. The zero value of each optional field is the
// real behaviour.
type Options struct {
	// StateDir is the state directory holding the runs.
	StateDir string
	// Apply removes files; without it nothing is touched and the report
	// says what would happen.
	Apply bool
	// Now gives the zip's date, in local time.
	Now func() time.Time
	// Scan is passed to the fresh scan, and to git.
	Scan scan.Options
	// Zips is where the zip is written and read back; nil is the disk.
	Zips ZipStore
}

// Left is a file retire did not remove, and why.
type Left struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Report is what retire did, or would do without --apply. Paths are
// relative to Root, with forward slashes.
type Report struct {
	Run  string `json:"run"`
	Root string `json:"root"`
	// Zip is the zip's absolute path, or null when there is none.
	Zip *string `json:"zip"`
	// Deleted are tracked files identical to HEAD, removed from the working
	// tree and recoverable from git.
	Deleted []string `json:"deleted"`
	// Zipped are files copied into the zip and then removed.
	Zipped []string `json:"zipped"`
	Left   []Left   `json:"left"`
	// RecoverableUnpushed are deleted files whose last commit is not on an
	// upstream, or that have none: recoverable from local commits only.
	RecoverableUnpushed []string `json:"recoverable_unpushed"`
	// ExcludeAdded is whether the zip's path went into the repository's
	// info/exclude, so git does not list it.
	ExcludeAdded bool `json:"exclude_added"`
	Applied      bool `json:"applied"`
}

// RefusedError is a run retire will not touch at all, with one sentence for
// a person. Nothing was changed.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// StoppedError is a retire that stopped part way. The report says exactly
// what was removed; every file not removed is still in place.
type StoppedError struct{ Msg string }

func (e *StoppedError) Error() string { return e.Msg }

// Reasons a file is left.
const (
	reasonNoRecord   = "the run holds no record of it"
	reasonNoID       = "Smrit returned no id for it"
	reasonBadPath    = "its path does not name a file inside the folder"
	reasonSymlink    = "it is now a symbolic link"
	reasonNotRegular = "it is no longer a regular file"
	reasonUnreadable = "it could not be read"
	reasonMissing    = "it is no longer in the folder"
	reasonTruncated  = "the scan stopped before reaching it"
	reasonKeep       = "it is a README, licence or instruction file, which is always kept"
	reasonUsed       = "a tracked file in its repository names it"
	reasonChanged    = "it changed since the import"
	reasonPrograms   = "its repository's git config names programs, so its git state cannot be trusted"
	reasonGitFailed  = "git could not read its repository, so its git state is unknown"
	reasonNoGit      = "git was not found, so its git state is unknown"
	reasonGitUnknown = "its git state is unknown"
	reasonDuring     = "it changed while retire ran"
	reasonStopped    = "retire stopped before removing it"
)

// item is a file retire will remove.
type item struct {
	path string
	sha  string
	// unpushed is a clean file whose last commit is on no upstream.
	unpushed bool
}

// Run retires run id. Without o.Apply it only reads.
func Run(ctx context.Context, id string, o Options) (Report, error) {
	rep := Report{Run: id, Deleted: []string{}, Zipped: []string{}, Left: []Left{}, RecoverableUnpushed: []string{}}
	missing := &RefusedError{"No import run has that id. The id is printed when a run begins."}
	p := ""
	if o.StateDir != "" {
		p = state.RunPath(o.StateDir, id)
	}
	if p == "" {
		return rep, missing
	}
	if _, err := os.Lstat(p); err != nil {
		return rep, missing
	}
	unlock, err := lockRun(o.StateDir, id)
	if err != nil {
		return rep, err
	}
	defer unlock()
	run, err := state.LoadRun(o.StateDir, id)
	if err != nil {
		return rep, &RefusedError{"The saved import run could not be read, so nothing was retired."}
	}
	rep.Root = run.Root
	if run.Phase != state.PhaseFinished {
		return rep, &RefusedError{fmt.Sprintf("The import run %s has not finished, so nothing was retired. Finish it with smrit import and --resume %s first.", id, id)}
	}

	res, err := scan.Run(ctx, []string{run.Root}, o.Scan)
	if err != nil {
		var re *scan.RefusedError
		var ue *scan.UsageError
		if errors.As(err, &re) || errors.As(err, &ue) {
			return rep, &RefusedError{"The run's folder cannot be scanned, so nothing was retired: " + err.Error()}
		}
		return rep, fmt.Errorf("The scan of the run's folder failed, so nothing was retired: %v", err)
	}
	if len(res.Roots) != 1 {
		return rep, &RefusedError{"The run's folder cannot be scanned, so nothing was retired."}
	}
	rootPath := res.Roots[0].Path
	rep.Root = rootPath

	zipItems, delItems := classify(run, res, &rep)

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return rep, &RefusedError{"The run's folder cannot be opened, so nothing was retired."}
	}
	defer root.Close()

	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	day := now().Local()
	zs := o.Zips
	if zs == nil {
		zs = disk{}
	}

	// The repository holding the folder, for the zip's name and its line
	// in info/exclude. One whose config names programs is not opened.
	var repo *scan.Repo
	if len(zipItems) > 0 {
		repo, _ = scan.OpenRepo(ctx, rootPath, o.Scan)
	}
	// A name is taken when anything is there, or when the repository
	// tracks it: a zip there would change a tracked file.
	taken := func(name string) (bool, error) {
		if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if repo == nil {
			return false, nil
		}
		rel, ok := repoRel(repo, rootPath, name)
		if !ok {
			return false, errors.New("the folder is not inside its repository")
		}
		tracked, err := repo.Tracked(ctx, rel)
		if err != nil {
			return false, errors.New("git could not say whether the zip's name is tracked")
		}
		return tracked, nil
	}

	if !o.Apply {
		for _, it := range delItems {
			rep.Deleted = append(rep.Deleted, it.path)
			if it.unpushed {
				rep.RecoverableUnpushed = append(rep.RecoverableUnpushed, it.path)
			}
		}
		for _, it := range zipItems {
			rep.Zipped = append(rep.Zipped, it.path)
		}
		if len(zipItems) > 0 {
			for n := 1; n <= maxSuffix; n++ {
				name := zipNameFor(day, n)
				if t, err := taken(name); err != nil {
					break
				} else if t {
					continue
				}
				zp := filepath.Join(rootPath, name)
				rep.Zip = &zp
				if repo != nil {
					if rel, ok := repoRel(repo, rootPath, name); ok {
						if ex, err := planExclude(ctx, repo, rel); err == nil && ex != nil {
							rep.ExcludeAdded = true
						}
					}
				}
				break
			}
		}
		return rep, nil
	}

	rep.Applied = true
	if err := ctx.Err(); err != nil {
		return rep, &StoppedError{"retire was stopped before it changed anything."}
	}
	// Leave everything in place: the plan is reported as left, with why.
	abandon := func(why string) (Report, error) {
		for _, it := range zipItems {
			rep.Left = append(rep.Left, Left{it.path, reasonStopped})
		}
		for _, it := range delItems {
			rep.Left = append(rep.Left, Left{it.path, reasonStopped})
		}
		return rep, &StoppedError{why + " Nothing was removed."}
	}

	// The zip first, written and read back; only then is anything removed.
	if len(zipItems) > 0 {
		name, err := writeZip(root, zs, day, taken, zipItems)
		if err == nil {
			err = verifyZip(root, zs, name, zipItems)
		}
		if err == nil && repo != nil {
			err = addExclude(ctx, repo, rootPath, name, &rep)
		}
		if err != nil {
			if name != "" {
				root.Remove(name)
			}
			return abandon(fmt.Sprintf("The zip could not be written and checked (%v), so it was deleted.", err))
		}
		zp := filepath.Join(rootPath, name)
		rep.Zip = &zp
	}

	// Zipped files are removed first, then the files git holds.
	type removal struct {
		it     item
		zipped bool
	}
	var order []removal
	for _, it := range zipItems {
		order = append(order, removal{it, true})
	}
	for _, it := range delItems {
		order = append(order, removal{it, false})
	}
	for i, r := range order {
		copyNote := ""
		if r.zipped {
			copyNote = "; a copy is in the zip"
		}
		// Read again just before removal: a file changed since it was
		// checked (or zipped) keeps its new bytes.
		if !unchanged(root, r.it) {
			rep.Left = append(rep.Left, Left{r.it.path, reasonDuring + copyNote})
			continue
		}
		if err := root.Remove(filepath.FromSlash(r.it.path)); err != nil {
			rep.Left = append(rep.Left, Left{r.it.path, "it could not be removed" + copyNote})
			for _, rest := range order[i+1:] {
				note := ""
				if rest.zipped {
					note = "; a copy is in the zip"
				}
				rep.Left = append(rep.Left, Left{rest.it.path, reasonStopped + note})
			}
			return rep, &StoppedError{fmt.Sprintf("retire stopped because %s could not be removed. The report lists what was removed and what is still in place.", r.it.path)}
		}
		if r.zipped {
			rep.Zipped = append(rep.Zipped, r.it.path)
			continue
		}
		rep.Deleted = append(rep.Deleted, r.it.path)
		if r.it.unpushed {
			rep.RecoverableUnpushed = append(rep.RecoverableUnpushed, r.it.path)
		}
	}
	return rep, nil
}

// classify decides, for each plan entry, whether the file is deleted,
// zipped or left, and records every left file in rep.
func classify(run *state.Run, res scan.Result, rep *Report) (zipItems, delItems []item) {
	files := map[string]scan.File{}
	for _, f := range res.Files {
		files[f.Path] = f
	}
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Path] = s.Reason
	}
	seen := map[string]bool{}
	for _, pe := range run.Plan.Entries {
		p := pe.Path
		if seen[p] {
			continue
		}
		seen[p] = true
		leave := func(reason string) { rep.Left = append(rep.Left, Left{p, reason}) }

		e, ok := run.Entries[p]
		switch {
		case !ok:
			leave(reasonNoRecord)
			continue
		case !done(e):
			leave(fmt.Sprintf("its import status is %s", e.Status))
			continue
		case e.ID == "":
			leave(reasonNoID)
			continue
		case !safePath(p):
			leave(reasonBadPath)
			continue
		}

		f, ok := files[p]
		if !ok {
			switch skipped[p] {
			case scan.SkipSymlink:
				leave(reasonSymlink)
			case scan.SkipNotRegular:
				leave(reasonNotRegular)
			case scan.SkipUnreadable:
				leave(reasonUnreadable)
			case "":
				if res.Truncated {
					leave(reasonTruncated)
				} else {
					leave(reasonMissing)
				}
			default:
				leave(reasonMissing)
			}
			continue
		}
		if slices.Contains(f.Flags, scan.FlagKeep) {
			leave(reasonKeep)
			continue
		}
		if slices.Contains(f.Flags, scan.FlagUsed) {
			leave(reasonUsed)
			continue
		}
		if e.SourceSHA256 == "" || f.SHA256 != e.SourceSHA256 {
			leave(reasonChanged)
			continue
		}

		it := item{path: p, sha: e.SourceSHA256}
		switch f.Git.State {
		case scan.StateClean:
			if f.Git.Repo == "" {
				leave(reasonGitUnknown)
				continue
			}
			it.unpushed = f.Git.Pushed == nil || !*f.Git.Pushed
			delItems = append(delItems, it)
		case scan.StateModified, scan.StateUntracked, scan.StateIgnored:
			zipItems = append(zipItems, it)
		case scan.StateOutside:
			switch f.Git.Unknown {
			case "":
				zipItems = append(zipItems, it)
			case scan.UnknownNamesPrograms:
				leave(reasonPrograms)
			case scan.UnknownUnreadable:
				leave(reasonGitFailed)
			case scan.UnknownNoGit:
				leave(reasonNoGit)
			default:
				leave(reasonGitUnknown)
			}
		default:
			leave(reasonGitUnknown)
		}
	}
	return zipItems, delItems
}

// done reports whether Smrit holds the entry: created, updated or
// unchanged, or for a workbook, already there.
func done(e state.EntryState) bool {
	return state.Done(e.Status) || (e.Kind == scan.KindWorkbook && e.Status == state.StatusExists)
}

// safePath reports whether p is a clean relative path with forward slashes
// that stays inside the folder.
func safePath(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

// openRegular opens rel inside root for reading, never through a symbolic
// link at its end, and only when it is a regular file. os.Root keeps every
// folder on the way inside the root; the final name is compared by Lstat
// with what was opened, because os.Root follows a link inside the root.
func openRegular(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	name := filepath.FromSlash(rel)
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	lst, err := root.Lstat(name)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() || !lst.Mode().IsRegular() || !os.SameFile(st, lst) {
		f.Close()
		return nil, nil, errors.New("not a regular file")
	}
	return f, st, nil
}

// unchanged reports whether the file at it.path is still a regular file
// whose bytes hash to it.sha.
func unchanged(root *os.Root, it item) bool {
	f, _, err := openRegular(root, it.path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == it.sha
}

// repoRel is name, a file in the folder, relative to the repository's top
// with forward slashes.
func repoRel(repo *scan.Repo, rootPath, name string) (string, bool) {
	rel, err := filepath.Rel(repo.Top, filepath.Join(rootPath, name))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// addExclude puts the zip into the repository's info/exclude when git does
// not already ignore it.
func addExclude(ctx context.Context, repo *scan.Repo, rootPath, name string, rep *Report) error {
	rel, ok := repoRel(repo, rootPath, name)
	if !ok {
		return nil
	}
	ex, err := planExclude(ctx, repo, rel)
	if err != nil || ex == nil {
		return err
	}
	if err := ex.apply(); err != nil {
		return err
	}
	rep.ExcludeAdded = true
	return nil
}

// lockRun holds the run's lock, so retire never runs beside an import or
// another retire of the same run.
func lockRun(dir, id string) (func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := state.Lock(ctx, filepath.Join(state.RunsDir(dir), id+".lock"), nil)
	if errors.Is(err, state.ErrBusy) {
		return nil, &RefusedError{"Another smrit command is using this run, so nothing was retired."}
	}
	if err != nil {
		return nil, errors.New("Could not lock the import run.")
	}
	return unlock, nil
}

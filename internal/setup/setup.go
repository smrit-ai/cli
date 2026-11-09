// Package setup writes on a person's machine what the onboarding skill
// otherwise has an agent edit by hand: a harness's Smrit server pinned to
// the signed in organization, the rule that allows its tools, and the always
// on block in the instruction files. These are the person's own files, so
// nothing is written without Apply, every change is shown first as a diff,
// every other entry is kept and a backup is made beside each file changed.
package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Where the always on block goes.
const (
	BlockEverywhere = "everywhere"
	BlockProject    = "project"
	BlockBoth       = "both"
)

// Refused is a change setup will not make; nothing has been written.
type Refused struct{ Msg string }

func (e *Refused) Error() string { return e.Msg }

// Partial means Apply did some of the work and not all of it.
type Partial struct{ Msg string }

func (e *Partial) Error() string { return e.Msg }

// Config is one run of setup.
type Config struct {
	Harness string
	// Origin is the compiled server address; Endpoint the validated
	// profile's /mcp/o/<slug>/<id>; Org its slug and OrgID its id.
	Origin, Endpoint, Org, OrgID string
	// Block is the always on block, from ExtractBlock.
	Block string
	// Where is BlockEverywhere, BlockProject or BlockBoth; Project the
	// absolute folder the project file goes in.
	Where, Project string
	Home           string
	// WorkDir is where harness commands run, so Claude Code's local scope
	// is the project's: the project folder, else the current one.
	WorkDir string
	// StateDir is the CLI's state folder; backups of project files go in
	// its backups folder, never in the working tree.
	StateDir string
	Getenv   func(string) string
	Exec     Exec
	Now      func() time.Time
}

// FileChange is one file setup would write.
type FileChange struct {
	Path string `json:"path"`
	// Kind is settings or instructions.
	Kind string `json:"kind"`
	// Action is create, update or unchanged.
	Action string `json:"action"`
	Diff   string `json:"diff,omitempty"`
	Backup string `json:"backup,omitempty"`
	// Written is true once Apply has replaced the file.
	Written bool `json:"written"`

	old, new []byte
	mode     fs.FileMode
	dirMode  fs.FileMode
	exists   bool
	// rel is the path inside the project, for a project file.
	rel string
}

// Command is one harness command setup would run.
type Command struct {
	Argv []string `json:"argv"`
	// Dir is the folder it runs in, when that matters to what it does.
	Dir  string `json:"dir,omitempty"`
	Ran  bool   `json:"ran"`
	Exit *int   `json:"exit,omitempty"`
	// Output is the end of what a failed command wrote.
	Output string `json:"output,omitempty"`

	// restore puts back the entry a replacement removed, if this add fails.
	restore []string
}

// Plan is everything one run would change, and after Apply, what it did.
type Plan struct {
	Harness      string `json:"harness"`
	Name         string `json:"name"`
	Organization string `json:"organization"`
	Address      string `json:"address"`
	Applied      bool   `json:"applied"`
	// Entry is what happens to the harness's smrit server: add, reuse,
	// replace, or manual when its command is not on PATH.
	Entry    string        `json:"entry"`
	Files    []*FileChange `json:"files"`
	Commands []*Command    `json:"commands"`
	Notes    []string      `json:"notes"`
	// Next is what is still owed after Apply: the harness's own approval.
	Next []string `json:"next"`
}

// Prepare reads everything setup needs and works out every change, writing
// nothing and running only commands that look. A Refused error means
// nothing may be changed at all.
func Prepare(ctx context.Context, c *Config) (*Plan, error) {
	h, ok := builtin(c.Harness)
	if !ok {
		return nil, fmt.Errorf("setup does not change %s by itself", c.Harness)
	}
	address := c.Origin + c.Endpoint
	p := &Plan{Harness: h.id, Name: h.name, Organization: c.Org, Address: address,
		Files: []*FileChange{}, Commands: []*Command{}, Notes: []string{}, Next: []string{}}

	if h.settings != nil {
		fc, err := settingsChange(h.settings(c), h.allow)
		if err != nil {
			return nil, err
		}
		p.Files = append(p.Files, fc)
	} else {
		p.Notes = append(p.Notes, h.name+" has no allow rule for a server's tools, so setup adds none. Codex asks for approval as its own settings say.")
	}

	var paths []string
	if c.Where == BlockEverywhere || c.Where == BlockBoth {
		paths = append(paths, h.global(c))
	}
	if c.Where == BlockProject || c.Where == BlockBoth {
		pp := filepath.Join(c.Project, h.projectFile)
		if len(paths) == 0 || paths[0] != pp {
			paths = append(paths, pp)
		}
	}
	for i, path := range paths {
		fc, err := blockChange(path, c.Block)
		if err != nil {
			return nil, err
		}
		if c.Where == BlockProject || (c.Where == BlockBoth && i == len(paths)-1) {
			if fc.rel, err = filepath.Rel(c.Project, path); err != nil {
				fc.rel = filepath.Base(path)
			}
		}
		p.Files = append(p.Files, fc)
	}

	if h == claudeCode {
		old := filepath.Join(claudeDir(c), "rules", "smrit.md")
		if _, err := os.Lstat(old); err == nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s is there from an earlier setup. setup leaves it alone; once CLAUDE.md holds the block, remove it yourself, or Claude Code reads the instructions twice.", old))
		}
	}

	entries, err := h.inspect(ctx, c)
	if err != nil {
		return nil, err
	}
	switch {
	case len(entries) == 1 && entries[0].missing:
		p.Entry = "manual"
		p.Commands = append(p.Commands, &Command{Argv: h.add(address, "")})
		p.Notes = append(p.Notes, fmt.Sprintf("%s's command, %s, was not found on PATH, so setup cannot check or change its smrit server. Run the command below yourself where %s is installed.", h.name, h.bin, h.name))
	case len(entries) == 0:
		p.Entry = "add"
		p.Commands = append(p.Commands, &Command{Argv: h.add(address, "")})
	default:
		if len(entries) > 1 {
			var found []string
			for _, e := range entries {
				found = append(found, fmt.Sprintf("in its %s scope at %s", e.scope, redact(e.url)))
			}
			p.Notes = append(p.Notes, fmt.Sprintf("%s has two smrit servers, %s. setup handles each in its own scope.", h.name, strings.Join(found, ", and ")))
		}
		// Every entry is decided before any command is planned, so one that
		// is refused leaves the others untouched too.
		actions := make([]string, len(entries))
		for i, e := range entries {
			if actions[i], err = classify(e.url, c, h, whose(h, e)); err != nil {
				return nil, err
			}
			if actions[i] != "replace" {
				continue
			}
			extras := e.extras
			if h.extras != nil {
				more, err := h.extras(c)
				if err != nil {
					return nil, err
				}
				extras = append(extras, more...)
			}
			if len(extras) > 0 {
				return nil, &Refused{fmt.Sprintf("%s smrit server points at %s and carries more than an address: %s. setup never removes or replaces it, because those would be lost. Change its address to %s by hand in %s, keeping them, then run setup again.", whose(h, e), redact(e.url), quotedList(extras), address, h.name)}
			}
			if h.remove != nil {
				if _, err := h.remove(e); err != nil {
					return nil, err
				}
			}
		}
		p.Entry = "reuse"
		for i, e := range entries {
			if actions[i] == "reuse" {
				p.Notes = append(p.Notes, fmt.Sprintf("%s smrit server already points at %s; setup leaves it as it is.", whose(h, e), address))
			} else {
				p.Entry = "replace"
				if h.remove != nil {
					rm, _ := h.remove(e)
					p.Commands = append(p.Commands, &Command{Argv: rm, Dir: localDir(rm, c)})
				}
				// In the scope it was found in: a project's own entry never
				// becomes one for every project.
				add := h.add(address, e.scope)
				cmd := &Command{Argv: add, Dir: localDir(add, c)}
				if h.remove != nil {
					cmd.restore = h.add(e.url, e.scope)
				}
				p.Commands = append(p.Commands, cmd)
				p.Notes = append(p.Notes, fmt.Sprintf("%s smrit server points at %s. setup pins it to %s, so a later sign in cannot land in another organization.", whose(h, e), redact(e.url), c.Org))
			}
			if e.disabled {
				p.Notes = append(p.Notes, fmt.Sprintf("%s smrit server is turned off. Turn it on in %s.", whose(h, e), h.name))
			}
		}
	}
	p.Next = append(p.Next, h.approval(c))
	return p, nil
}

// whose names an entry for a message: the harness, and the scope when the
// harness has scopes.
func whose(h *harness, e entry) string {
	if e.scope != "" {
		return fmt.Sprintf("%s's %s scope", h.name, e.scope)
	}
	return h.name + "'s"
}

var pinnedPath = regexp.MustCompile(`^/mcp/o/([^/]+)/([^/]+)$`)

// classify decides what to do with an existing smrit server at raw. It is
// reused when it is the pinned address, and replaced when it is Smrit's
// bare address or this organization's under an older slug. Anything else is
// refused: another host, another organization, a path setup does not
// recognise, or an address carrying a user, a query or a fragment.
func classify(raw string, c *Config, h *harness, who string) (string, error) {
	pinned := c.Origin + c.Endpoint
	if raw == pinned || raw == pinned+"/" {
		return "reuse", nil
	}
	leave := func(why string) (string, error) {
		return "", &Refused{fmt.Sprintf("%s smrit server is at %s, %s. setup leaves it alone: change or remove it by hand in %s, then run setup again.", who, redact(raw), why, h.name)}
	}
	u, err1 := url.Parse(raw)
	o, err2 := url.Parse(c.Origin)
	if raw == "" || err1 != nil || err2 != nil || u.Scheme != o.Scheme || !strings.EqualFold(u.Host, o.Host) {
		return leave("which is not Smrit's address")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return leave("which carries more than Smrit's address")
	}
	switch path := u.Path; {
	case path == "" || path == "/" || path == "/mcp" || path == "/mcp/":
		return "replace", nil
	case strings.HasPrefix(path, "/mcp/o/"):
		m := pinnedPath.FindStringSubmatch(strings.TrimSuffix(path, "/"))
		if m != nil && m[2] == c.OrgID {
			return "replace", nil
		}
		if m != nil {
			return "", &Refused{fmt.Sprintf("%s smrit server is pinned to another organization, %s. setup leaves it alone: sign in to that one with smrit login and run setup with --org, or remove the entry in %s first.", who, quoted(m[1]), h.name)}
		}
		return leave("an organization address setup does not recognise")
	}
	return leave("a path setup does not recognise")
}

// redact is an address, quoted, without anything that could be a
// credential: no user, no query and no fragment. An address that does not
// parse is cut at its first ? or # and at 200 bytes, and quoted, so nothing
// from a config file reaches the terminal unescaped.
func redact(raw string) string {
	s := raw
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		s = u.Scheme + "://" + u.Host + u.EscapedPath()
	} else if at, scheme := strings.LastIndex(s, "@"), strings.Index(s, "://"); at >= 0 && scheme >= 0 && scheme < at {
		s = s[:scheme+3] + s[at+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return quoted(s)
}

// quoted is a value read from someone else's file, escaped for printing.
func quoted(s string) string { return strconv.Quote(s) }

func quotedList(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = quoted(k)
	}
	return strings.Join(q, ", ")
}

// localDir is where a Claude Code local scope command runs, shown beside
// it; "" for every other command.
func localDir(argv []string, c *Config) string {
	for _, a := range argv {
		if a == "local" {
			return c.WorkDir
		}
	}
	return ""
}

// readTarget reads a file setup may write. A link or anything but a plain
// file is refused: setup writes the file itself, never through a link.
func readTarget(path string) (data []byte, mode fs.FileMode, exists bool, err error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, &Refused{fmt.Sprintf("Could not read %s: %v", path, err)}
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		return nil, 0, false, &Refused{fmt.Sprintf("%s is a symbolic link. setup does not write through links: make the change by hand, or put the file itself there and run setup again.", path)}
	}
	if !st.Mode().IsRegular() {
		return nil, 0, false, &Refused{fmt.Sprintf("%s is not a plain file, so setup leaves it alone.", path)}
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, 0, false, &Refused{fmt.Sprintf("Could not read %s: %v", path, err)}
	}
	return data, st.Mode().Perm(), true, nil
}

func change(path, kind string, old, new []byte, mode fs.FileMode, exists bool) *FileChange {
	fc := &FileChange{Path: path, Kind: kind, old: old, new: new, mode: mode, exists: exists}
	switch {
	case !exists:
		fc.Action = "create"
		fc.Diff = unifiedDiff("/dev/null", path, "", string(new))
	case string(old) == string(new):
		fc.Action = "unchanged"
	default:
		fc.Action = "update"
		fc.Diff = unifiedDiff(path, path, string(old), string(new))
	}
	return fc
}

func settingsChange(path, rule string) (*FileChange, error) {
	old, mode, exists, err := readTarget(path)
	if err != nil {
		return nil, err
	}
	crlf := bytes.Contains(old, []byte("\r\n"))
	out, changed, err := AddToList(old, []string{"permissions", "allow"}, rule)
	if errors.Is(err, ErrNotJSON) {
		return nil, &Refused{fmt.Sprintf("%s is not valid JSON, so setup leaves it alone. Fix it, then run setup again.", path)}
	}
	if err != nil {
		return nil, &Refused{fmt.Sprintf("setup leaves %s alone: %v.", path, err)}
	}
	if changed && crlf {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	if !exists {
		mode = 0o600
	}
	fc := change(path, "settings", old, out, mode, exists)
	fc.dirMode = 0o700
	return fc, nil
}

func blockChange(path, block string) (*FileChange, error) {
	old, mode, exists, err := readTarget(path)
	if err != nil {
		return nil, err
	}
	out, err := ApplyBlock(string(old), block)
	if errors.Is(err, ErrNoBlock) {
		return nil, err
	}
	if err != nil {
		return nil, &Refused{fmt.Sprintf("setup leaves %s alone: %v. Fix the block by hand, then run setup again.", path, err)}
	}
	if !exists {
		mode = 0o644
	}
	fc := change(path, "instructions", old, []byte(out), mode, exists)
	fc.dirMode = 0o755
	return fc, nil
}

// Apply writes each changed file and runs each command.
//
// A file is written only while it still holds what Prepare read: Prepare
// may wait minutes on a harness command, and anything another program
// wrote meanwhile must not be lost. So every file is checked first, and a
// change refuses the whole run before anything is written; each file is
// read again just before its backup, and the bytes backed up are the bytes
// on disk. A new file is linked into place, never over a file that has
// appeared. A command that fails is reported, and a replacement whose add
// failed puts the old entry back.
func (p *Plan) Apply(ctx context.Context, c *Config) error {
	p.Applied = true
	stamp := c.Now().UTC().Format("20060102T150405Z")
	var changed []*FileChange
	for _, f := range p.Files {
		if f.Action != "unchanged" {
			changed = append(changed, f)
		}
	}
	for _, f := range changed {
		if err := f.unchangedOnDisk(); err != nil {
			return &Refused{err.Error() + " setup wrote nothing and ran nothing. Run it again to see the new diff."}
		}
	}
	for _, f := range changed {
		if f.exists {
			name, err := f.backup(c, stamp)
			if err != nil {
				var refused *Refused
				if errors.As(err, &refused) {
					return &Refused{refused.Msg + " setup left it as it is" + writtenSoFar(changed) + ". Run setup again to see the new diff."}
				}
				return &Partial{fmt.Sprintf("Could not back up %s, so it was not changed: %v", f.Path, err)}
			}
			f.Backup = name
			if err := writeAtomic(f.Path, f.new, f.mode); err != nil {
				return &Partial{fmt.Sprintf("Could not write %s: %v", f.Path, err)}
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(f.Path), f.dirMode); err != nil {
				return &Partial{fmt.Sprintf("Could not create the folder for %s: %v", f.Path, err)}
			}
			if err := writeNew(f.Path, f.new, f.mode); errors.Is(err, fs.ErrExist) {
				return &Refused{fmt.Sprintf("%s appeared after setup read it. setup left it as it is%s. Run setup again to see the new diff.", f.Path, writtenSoFar(changed))}
			} else if err != nil {
				return &Partial{fmt.Sprintf("Could not write %s: %v", f.Path, err)}
			}
		}
		f.Written = true
	}
	if p.Entry == "manual" {
		return &Partial{fmt.Sprintf("The files are written, but %s's smrit server is not set up: run %s yourself.", p.Name, Display(p.Commands[0].Argv))}
	}
	for _, cmd := range p.Commands {
		out, errs, code, err := c.Exec(ctx, c.WorkDir, cmd.Argv)
		cmd.Ran = err == nil
		if err == nil {
			cmd.Exit = &code
		}
		if err == nil && code == 0 {
			continue
		}
		cmd.Output = tail(errs + out)
		msg := fmt.Sprintf("%s failed", Display(cmd.Argv))
		if err != nil {
			msg = fmt.Sprintf("Could not run %s: %v", Display(cmd.Argv), err)
		}
		if cmd.restore != nil {
			if _, _, rc, rerr := c.Exec(ctx, c.WorkDir, cmd.restore); rerr == nil && rc == 0 {
				msg += "; the previous smrit server was put back"
			} else {
				msg += fmt.Sprintf("; putting the previous smrit server back failed too, so run %s yourself", Display(cmd.restore))
			}
		}
		return &Partial{msg + "."}
	}
	return nil
}

// unchangedOnDisk checks that the file is still what Prepare read: still a
// plain file with the same bytes, or still absent.
func (f *FileChange) unchangedOnDisk() error {
	now, _, exists, err := readTarget(f.Path)
	if err != nil {
		return err
	}
	if exists != f.exists || !bytes.Equal(now, f.old) {
		return &Refused{fmt.Sprintf("%s changed after setup read it.", f.Path)}
	}
	return nil
}

func writtenSoFar(files []*FileChange) string {
	var done []string
	for _, f := range files {
		if f.Written {
			done = append(done, f.Path)
		}
	}
	if len(done) == 0 {
		return ", and wrote nothing"
	}
	return "; it had already written " + strings.Join(done, ", ")
}

// backup reads the file again and copies the bytes on disk, with its mode,
// refusing when they are no longer what Prepare read. A global file's
// backup goes beside it; a project file's under the state folder,
// backups/<stamp>/<path in the project>, so no copy lands in a working tree.
func (f *FileChange) backup(c *Config, stamp string) (string, error) {
	data, _, exists, err := readTarget(f.Path)
	if err != nil {
		return "", err
	}
	if !exists || !bytes.Equal(data, f.old) {
		return "", &Refused{fmt.Sprintf("%s changed after setup read it.", f.Path)}
	}
	base := f.Path + ".smrit-backup-" + stamp
	if f.rel != "" {
		if c.StateDir == "" {
			return "", errors.New("there is no state folder to keep the backup in")
		}
		base = filepath.Join(c.StateDir, "backups", stamp, f.rel)
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			return "", err
		}
	}
	return backup(base, data, f.mode)
}

// writeNew creates path with data and mode, linking a finished temporary
// file into place so a file that appeared meanwhile is never replaced: that
// is fs.ErrExist.
func writeNew(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = errors.Join(tmp.Chmod(mode), write(tmp, data), tmp.Sync())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Link(name, path)
}

func write(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// tail is the last few hundred bytes a failed command wrote. The commands
// setup runs carry no credential, so this holds none of the CLI's.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// backup writes data at base with mode, or at base-1, base-2 and so on
// when that name is taken: a backup never replaces another file.
func backup(base string, data []byte, mode fs.FileMode) (string, error) {
	for n := 0; n < 100; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		serr := f.Sync()
		cerr := f.Close()
		if err := errors.Join(werr, serr, cerr, os.Chmod(name, mode)); err != nil {
			os.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", errors.New("too many backups with the same time")
}

// writeAtomic replaces path with data: a temporary file in the same folder,
// given the mode, synced, then renamed over it.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// Display is argv as a person would type it in a shell. It is only ever
// shown, never run.
func Display(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if plainArg.MatchString(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

// String is the plan as text: each diff, each command, the notes, and what
// is still owed.
func (p *Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, for %s at %s\n", p.Name, p.Organization, p.Address)
	var unchanged []string
	for _, f := range p.Files {
		if f.Action == "unchanged" {
			unchanged = append(unchanged, f.Path)
			continue
		}
		b.WriteString("\n" + f.Diff)
		if f.Backup != "" {
			fmt.Fprintf(&b, "Backed up the old file to %s\n", f.Backup)
		}
	}
	if len(unchanged) > 0 {
		b.WriteString("\nAlready up to date:\n")
		for _, u := range unchanged {
			fmt.Fprintf(&b, "  %s\n", u)
		}
	}
	if len(p.Commands) > 0 {
		if p.Applied && p.Entry != "manual" {
			b.WriteString("\nRan:\n")
		} else {
			b.WriteString("\nCommands:\n")
		}
		for _, c := range p.Commands {
			fmt.Fprintf(&b, "  %s", Display(c.Argv))
			if c.Dir != "" {
				fmt.Fprintf(&b, "  (in %s)", c.Dir)
			}
			switch {
			case c.Exit != nil && *c.Exit != 0:
				fmt.Fprintf(&b, "  (failed, exit %d)", *c.Exit)
			case p.Applied && !c.Ran && p.Entry != "manual":
				b.WriteString("  (not run)")
			}
			b.WriteString("\n")
		}
	}
	if len(p.Notes) > 0 {
		b.WriteString("\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "%s\n", n)
		}
	}
	changes := len(p.Commands) > 0 && p.Entry != "manual"
	for _, f := range p.Files {
		changes = changes || f.Action != "unchanged"
	}
	b.WriteString("\n")
	switch {
	case !p.Applied && !changes:
		b.WriteString("Nothing to change.\n")
	case !p.Applied:
		b.WriteString("Nothing was written. Run again with --apply to make these changes.\n")
	default:
		if len(p.Next) > 0 {
			b.WriteString("Still to do:\n")
			for _, n := range p.Next {
				fmt.Fprintf(&b, "  %s\n", n)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

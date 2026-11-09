// Package setup writes on a person's machine what the onboarding skill
// otherwise has an agent edit by hand: a harness's Smrit server pinned to
// the signed in organization, the rule that allows its tools, and the always
// on block in the instruction files. These are the person's own files, so
// nothing is written without Apply, every change is shown first as a diff,
// every other entry is kept and a backup is made beside each file changed.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	Getenv         func(string) string
	Exec           Exec
	Now            func() time.Time
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
}

// Command is one harness command setup would run.
type Command struct {
	Argv []string `json:"argv"`
	Ran  bool     `json:"ran"`
	Exit *int     `json:"exit,omitempty"`
	// Output is the end of what a failed command wrote.
	Output string `json:"output,omitempty"`
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

	// restore puts back the entry a replacement removed, if the add fails.
	restore []string
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
	for _, path := range paths {
		fc, err := blockChange(path, c.Block)
		if err != nil {
			return nil, err
		}
		p.Files = append(p.Files, fc)
	}

	if h == claudeCode {
		old := filepath.Join(claudeDir(c), "rules", "smrit.md")
		if _, err := os.Lstat(old); err == nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s is there from an earlier setup. setup leaves it alone; once CLAUDE.md holds the block, remove it yourself, or Claude Code reads the instructions twice.", old))
		}
	}

	e, err := h.inspect(ctx, c)
	if err != nil {
		return nil, err
	}
	switch {
	case e.missing:
		p.Entry = "manual"
		p.Commands = append(p.Commands, &Command{Argv: h.add(address, "")})
		p.Notes = append(p.Notes, fmt.Sprintf("%s's command, %s, was not found on PATH, so setup cannot check or change its smrit server. Run the command below yourself where %s is installed.", h.name, h.bin, h.name))
	case !e.present:
		p.Entry = "add"
		p.Commands = append(p.Commands, &Command{Argv: h.add(address, "")})
	default:
		action, err := classify(e.url, c, h)
		if err != nil {
			return nil, err
		}
		switch action {
		case "reuse":
			p.Entry = "reuse"
			p.Notes = append(p.Notes, fmt.Sprintf("%s's smrit server already points at %s; setup leaves it as it is.", h.name, address))
		case "replace":
			p.Entry = "replace"
			if h.remove != nil {
				rm, err := h.remove(e)
				if err != nil {
					return nil, err
				}
				p.Commands = append(p.Commands, &Command{Argv: rm})
				p.restore = h.add(e.url, e.scope)
			}
			p.Commands = append(p.Commands, &Command{Argv: h.add(address, "")})
			p.Notes = append(p.Notes, fmt.Sprintf("%s's smrit server points at %s. setup pins it to %s, so a later sign in cannot land in another organization.", h.name, redact(e.url), c.Org))
		}
		if e.disabled {
			p.Notes = append(p.Notes, fmt.Sprintf("%s's smrit server is turned off. Turn it on in %s, for example with %s mcp enable smrit where that exists.", h.name, h.name, h.bin))
		}
	}
	p.Next = append(p.Next, h.approval(c))
	return p, nil
}

var pinnedPath = regexp.MustCompile(`^/mcp/o/([^/]+)/([^/]+)/?$`)

// classify decides what to do with an existing smrit server at raw: reuse it
// when it is the pinned address, replace it when it is another address of
// Smrit's own for this organization or none, and refuse anything else.
func classify(raw string, c *Config, h *harness) (string, error) {
	pinned := c.Origin + c.Endpoint
	if raw == pinned || raw == pinned+"/" {
		return "reuse", nil
	}
	u, err1 := url.Parse(raw)
	o, err2 := url.Parse(c.Origin)
	if raw == "" || err1 != nil || err2 != nil || u.Scheme != o.Scheme || !strings.EqualFold(u.Host, o.Host) || u.User != nil {
		where := "something that is not Smrit's address"
		if raw != "" && err1 == nil {
			where = redact(raw) + ", which is not Smrit's address"
		}
		return "", &Refused{fmt.Sprintf("%s already has a server named smrit, at %s. setup leaves it alone: rename or remove it in %s, then run setup again.", h.name, where, h.name)}
	}
	if m := pinnedPath.FindStringSubmatch(u.Path); m != nil && m[2] != c.OrgID {
		return "", &Refused{fmt.Sprintf("%s's smrit server is pinned to another organization, %s. setup leaves it alone: sign in to that one with smrit login --org %s and run setup with --org %s, or remove the entry in %s first.", h.name, m[1], m[1], m[1], h.name)}
	}
	return "replace", nil
}

// redact is an address without anything that could be a credential: no
// user, no query and no fragment.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "an address setup cannot read"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
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
	out, _, err := AddToList(old, []string{"permissions", "allow"}, rule)
	if errors.Is(err, ErrNotJSON) {
		return nil, &Refused{fmt.Sprintf("%s is not valid JSON, so setup leaves it alone. Fix it, then run setup again.", path)}
	}
	if err != nil {
		return nil, &Refused{fmt.Sprintf("setup leaves %s alone: %v.", path, err)}
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

// Apply writes each changed file, a backup of it first, and runs each
// command. It stops at the first file it cannot write. A command that fails
// is reported, and a replacement whose add failed puts the old entry back.
func (p *Plan) Apply(ctx context.Context, c *Config) error {
	p.Applied = true
	stamp := c.Now().UTC().Format("20060102T150405Z")
	for _, f := range p.Files {
		if f.Action == "unchanged" {
			continue
		}
		if f.exists {
			name, err := backup(f.Path, f.old, f.mode, stamp)
			if err != nil {
				return &Partial{fmt.Sprintf("Could not back up %s, so it was not changed: %v", f.Path, err)}
			}
			f.Backup = name
		} else if err := os.MkdirAll(filepath.Dir(f.Path), f.dirMode); err != nil {
			return &Partial{fmt.Sprintf("Could not create the folder for %s: %v", f.Path, err)}
		}
		if err := writeAtomic(f.Path, f.new, f.mode); err != nil {
			return &Partial{fmt.Sprintf("Could not write %s: %v", f.Path, err)}
		}
		f.Written = true
	}
	if p.Entry == "manual" {
		return &Partial{fmt.Sprintf("The files are written, but %s's smrit server is not set up: run %s yourself.", p.Name, Display(p.Commands[0].Argv))}
	}
	for i, cmd := range p.Commands {
		out, errs, code, err := c.Exec(ctx, cmd.Argv)
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
		if i > 0 && p.restore != nil {
			if _, _, rc, rerr := c.Exec(ctx, p.restore); rerr == nil && rc == 0 {
				msg += "; the previous smrit server was put back"
			} else {
				msg += fmt.Sprintf("; putting the previous smrit server back failed too, so run %s yourself", Display(p.restore))
			}
		}
		return &Partial{msg + "."}
	}
	return nil
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

// backup copies the file's old bytes beside it, with its mode, under a name
// no other file has.
func backup(path string, data []byte, mode fs.FileMode, stamp string) (string, error) {
	base := path + ".smrit-backup-" + stamp
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

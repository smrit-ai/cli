package importer

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/smrit-ai/cli/internal/state"
)

// ResumeError is a run that cannot be started or resumed as asked, with one
// sentence for a person. Nothing was sent.
type ResumeError struct{ Msg string }

func (e *ResumeError) Error() string { return e.Msg }

// begin loads the run to resume, or starts a new one and saves it at once,
// and holds the run's lock until the returned unlock is called, so two
// imports never write one run.
func (r *runner) begin() (unlock func(), err error) {
	dir := r.o.StateDir
	if dir == "" {
		return nil, errors.New("Could not find the state directory to keep the run in.")
	}
	if err := state.EnsureDir(state.RunsDir(dir)); err != nil {
		return nil, err
	}
	if r.o.Resume != "" {
		return r.resume(dir)
	}
	now, random := time.Now, r.o.Random
	if r.o.Now != nil {
		now = r.o.Now
	}
	if random == nil {
		random = rand.Reader
	}
	for range 3 {
		id, err := state.NewRunID(now(), random)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(state.RunPath(dir, id)); err == nil {
			continue
		}
		unlock, err := lockRun(dir, id)
		if err != nil {
			return nil, err
		}
		r.st = &state.Run{ID: id, Root: r.p.Root, Org: r.pr.org, PlanSHA256: r.o.PlanSHA256, Plan: r.p,
			Entries: map[string]state.EntryState{}, Phase: state.PhaseObjects}
		if err := r.save(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	}
	return nil, errors.New("Could not make a run id.")
}

func (r *runner) resume(dir string) (func(), error) {
	id := r.o.Resume
	if state.RunPath(dir, id) == "" {
		_, err := state.LoadRun(dir, id)
		return nil, &ResumeError{err.Error()}
	}
	unlock, err := lockRun(dir, id)
	if err != nil {
		return nil, err
	}
	st, err := state.LoadRun(dir, id)
	refuse := func(msg string) (func(), error) {
		unlock()
		return nil, &ResumeError{msg}
	}
	switch {
	case err != nil:
		return refuse(err.Error())
	case st.PlanSHA256 != r.o.PlanSHA256:
		return refuse("The plan changed since this run began, so it cannot be resumed. Run smrit import without --resume to start a new run.")
	case st.Root != r.p.Root:
		return refuse("This run imported from another folder, so it cannot be resumed with this plan.")
	case st.Org.ID != r.pr.org.ID:
		return refuse("This run imported into another organization, so it cannot be resumed here.")
	}
	r.st = st
	return unlock, nil
}

// lockRun takes the run's lock without waiting: a run already held is
// another import still running it.
func lockRun(dir, id string) (func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := state.Lock(ctx, filepath.Join(state.RunsDir(dir), id+".lock"), nil)
	if errors.Is(err, state.ErrBusy) {
		return nil, &ResumeError{"Another smrit import is running this run."}
	}
	if err != nil {
		return nil, errors.New("Could not lock the import run.")
	}
	return unlock, nil
}

func (r *runner) save() error { return state.SaveRun(r.o.StateDir, r.st) }

// Package profile remembers which organizations this machine is signed in to.
// A profile holds no secret: the tokens live in the store, keyed by the
// profile's organization slug.
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/smrit-ai/cli/internal/state"
)

const (
	fileName = "profiles.json"
	lockName = "profiles.lock"
	lockWait = 10 * time.Second
)

// Profile is one signed in organization. Nothing here is secret.
type Profile struct {
	// Org is the organization's slug, the key every profile is stored by.
	Org string `json:"org"`
	// OrgID is the organization's 21 character id.
	OrgID string `json:"org_id"`
	// Endpoint is the organization's MCP path, /mcp/o/<slug>/<id>.
	Endpoint string `json:"endpoint"`
	// ClientID is this CLI's OAuth registration for the profile.
	ClientID string `json:"client_id"`
	// RedirectURI is the loopback address the client was registered with.
	RedirectURI string `json:"redirect_uri"`
	// TokenEndpoint and RevocationEndpoint were discovered at sign in.
	TokenEndpoint      string `json:"token_endpoint"`
	RevocationEndpoint string `json:"revocation_endpoint,omitempty"`
}

var (
	// ErrNoProfile means there is no sign in to pick.
	ErrNoProfile = errors.New("Not signed in. Run smrit login.")
	// ErrSeveralProfiles means no organization was named and there is more
	// than one to choose from.
	ErrSeveralProfiles = errors.New("Signed in to several organizations. Pass --org to choose one.")
	// ErrDamagedProfile means a saved profile does not have the shape this
	// CLI writes, so nothing in it is trusted.
	ErrDamagedProfile = errors.New("A saved profile is damaged. Sign in again.")
)

// pickError carries a message that names the organizations, and still
// matches its sentinel with errors.Is.
type pickError struct {
	kind error
	msg  string
}

func (e *pickError) Error() string        { return e.msg }
func (e *pickError) Is(target error) bool { return target == e.kind }

var (
	segment = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)
	idShape = regexp.MustCompile(`^[A-Za-z0-9]{21}$`)
)

// ValidSlug reports whether s can name a profile: one path segment of the
// shape the server accepts, not starting with a dot.
func ValidSlug(s string) bool {
	return segment.MatchString(s) && !strings.HasPrefix(s, ".")
}

// ValidID reports whether s has the shape of an organization id.
func ValidID(s string) bool { return idShape.MatchString(s) }

// EndpointPath is the MCP path of an organization.
func EndpointPath(slug, id string) string { return "/mcp/o/" + slug + "/" + id }

// Path is the profiles file.
func Path() (string, error) {
	dir, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

type file struct {
	Profiles []Profile `json:"profiles"`
}

func read(path string) ([]Profile, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Could not read the profiles file: %w", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("The profiles file %s is not readable JSON.", path)
	}
	return f.Profiles, nil
}

// check refuses a profile that does not have the shape this CLI writes. The
// endpoint is joined to the server address with a token on the request, so
// it must be exactly the organization's own path, derived from a checked
// slug and id, and never a stored string taken on trust.
func check(p Profile) error {
	if !ValidSlug(p.Org) || !ValidID(p.OrgID) || p.Endpoint != EndpointPath(p.Org, p.OrgID) {
		name := p.Org
		if !ValidSlug(name) {
			name = "an organization"
		}
		return &pickError{ErrDamagedProfile, fmt.Sprintf(
			"The saved profile for %s is damaged. Run smrit logout, or delete the profiles file, and sign in again.", name)}
	}
	return nil
}

// Load returns every profile, sorted by organization. If any profile is
// damaged it returns ErrDamagedProfile and none of them.
func Load() ([]Profile, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	all, err := read(path)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if err := check(p); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// Pick returns the profile for org. With org empty it returns the only
// profile, ErrNoProfile when there is none, and ErrSeveralProfiles, naming
// them, when there are more.
func Pick(org string) (Profile, error) {
	all, err := Load()
	if err != nil {
		return Profile{}, err
	}
	if org != "" {
		for _, p := range all {
			if p.Org == org {
				return p, nil
			}
		}
		return Profile{}, &pickError{ErrNoProfile, fmt.Sprintf("Not signed in to %s. Run smrit login --org %s.", org, org)}
	}
	switch len(all) {
	case 0:
		return Profile{}, ErrNoProfile
	case 1:
		return all[0], nil
	}
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Org
	}
	return Profile{}, &pickError{ErrSeveralProfiles, fmt.Sprintf(
		"Signed in to several organizations: %s. Pass --org to choose one.", strings.Join(names, ", "))}
}

func update(change func([]Profile) []Profile) error {
	dir, err := state.Ensure()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	unlock, err := state.Lock(ctx, filepath.Join(dir, lockName), nil)
	if err != nil {
		return fmt.Errorf("Could not lock the profiles file: %w", err)
	}
	defer unlock()
	path := filepath.Join(dir, fileName)
	all, err := read(path)
	if err != nil {
		return err
	}
	all = change(all)
	sort.Slice(all, func(a, b int) bool { return all[a].Org < all[b].Org })
	data, err := json.MarshalIndent(file{Profiles: all}, "", "  ")
	if err != nil {
		return err
	}
	if err := state.WriteFile(path, append(data, '\n')); err != nil {
		return fmt.Errorf("Could not write the profiles file: %w", err)
	}
	return nil
}

// Save adds p, replacing any profile for the same organization.
func Save(p Profile) error {
	if !ValidSlug(p.Org) {
		return fmt.Errorf("%q is not an organization name this CLI can keep.", p.Org)
	}
	if !ValidID(p.OrgID) {
		return errors.New("The organization id is not 21 letters and digits.")
	}
	if err := check(p); err != nil {
		return err
	}
	return update(func(all []Profile) []Profile {
		out := all[:0]
		for _, q := range all {
			if q.Org != p.Org {
				out = append(out, q)
			}
		}
		return append(out, p)
	})
}

// Remove forgets the profile for org. A missing profile is not an error.
func Remove(org string) error {
	return update(func(all []Profile) []Profile {
		out := all[:0]
		for _, q := range all {
			if q.Org != org {
				out = append(out, q)
			}
		}
		return out
	})
}

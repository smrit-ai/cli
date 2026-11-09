package profile

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const orgID = "AbCdEfGhIjKlMnOpQrStU"

func home(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
}

func sample(slug string) Profile {
	return Profile{
		Org:           slug,
		OrgID:         orgID,
		Endpoint:      EndpointPath(slug, orgID),
		ClientID:      "client-1",
		RedirectURI:   "http://127.0.0.1:5555/callback",
		TokenEndpoint: "https://mcp.smrit.ai/oauth/token",
	}
}

func TestPick(t *testing.T) {
	home(t)
	if _, err := Pick(""); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("Pick with nothing saved: %v, want ErrNoProfile", err)
	}
	if err := Save(sample("acme")); err != nil {
		t.Fatal(err)
	}
	p, err := Pick("")
	if err != nil || p.Org != "acme" {
		t.Fatalf("Pick one: %+v, %v", p, err)
	}
	if err := Save(sample("beta")); err != nil {
		t.Fatal(err)
	}
	_, err = Pick("")
	if !errors.Is(err, ErrSeveralProfiles) {
		t.Fatalf("Pick among two: %v, want ErrSeveralProfiles", err)
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("the error does not name both organizations: %v", err)
	}
	if p, err := Pick("beta"); err != nil || p.Org != "beta" {
		t.Fatalf("Pick beta: %+v, %v", p, err)
	}
	if _, err := Pick("zeta"); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("Pick unknown: %v, want ErrNoProfile", err)
	}
	if err := Remove("acme"); err != nil {
		t.Fatal(err)
	}
	all, err := Load()
	if err != nil || len(all) != 1 || all[0].Org != "beta" {
		t.Fatalf("after Remove: %+v, %v", all, err)
	}
	if p, err := Pick(""); err != nil || p.Org != "beta" {
		t.Fatalf("Pick the one left: %+v, %v", p, err)
	}
}

func TestSaveReplacesByOrg(t *testing.T) {
	home(t)
	a := sample("acme")
	if err := Save(a); err != nil {
		t.Fatal(err)
	}
	a.ClientID = "client-2"
	if err := Save(a); err != nil {
		t.Fatal(err)
	}
	all, err := Load()
	if err != nil || len(all) != 1 || all[0].ClientID != "client-2" {
		t.Fatalf("Load: %+v, %v", all, err)
	}
}

func TestSaveRefusesBadNames(t *testing.T) {
	home(t)
	for _, slug := range []string{"", "..", ".hidden", "a/b", "a b", strings.Repeat("a", 65)} {
		p := sample("acme")
		p.Org = slug
		if err := Save(p); err == nil {
			t.Errorf("Save accepted org %q", slug)
		}
	}
	p := sample("acme")
	p.OrgID = "short"
	if err := Save(p); err == nil {
		t.Error("Save accepted an org id that is not 21 characters")
	}
}

func TestProfileFileIs0600(t *testing.T) {
	home(t)
	if err := Save(sample("acme")); err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("profiles file mode %o", st.Mode().Perm())
	}
}

func TestTamperedProfileIsRefused(t *testing.T) {
	cases := map[string]func(p *Profile){
		"endpoint for another org": func(p *Profile) { p.Endpoint = EndpointPath("evil", orgID) },
		"endpoint with navigation": func(p *Profile) { p.Endpoint = "/mcp/o/acme/../../steal" },
		"endpoint on another host": func(p *Profile) { p.Endpoint = "https://evil.example/mcp" },
		"org that is a path":       func(p *Profile) { p.Org = "../x"; p.Endpoint = EndpointPath(p.Org, p.OrgID) },
		"org id of the wrong shape": func(p *Profile) {
			p.OrgID = "short"
			p.Endpoint = EndpointPath(p.Org, p.OrgID)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			home(t)
			if err := Save(sample("beta")); err != nil {
				t.Fatal(err)
			}
			bad := sample("acme")
			tamper(&bad)
			path, _ := Path()
			raw, _ := json.Marshal(file{Profiles: []Profile{bad, sample("beta")}})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(); !errors.Is(err, ErrDamagedProfile) {
				t.Fatalf("Load: %v, want ErrDamagedProfile", err)
			}
			for _, org := range []string{"", "acme", "beta"} {
				if _, err := Pick(org); !errors.Is(err, ErrDamagedProfile) {
					t.Fatalf("Pick(%q): %v, want ErrDamagedProfile", org, err)
				}
			}
			if err := Remove(bad.Org); err != nil {
				t.Fatalf("Remove the damaged profile: %v", err)
			}
			if p, err := Pick(""); err != nil || p.Org != "beta" {
				t.Fatalf("after removing it: %+v, %v", p, err)
			}
		})
	}
}

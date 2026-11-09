package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/upgrade"
)

// latestServer answers releases/latest with a redirect to v9.8.7 and
// counts every other request.
func latestServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	other := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/smrit-ai/cli/releases/tag/v9.8.7", http.StatusFound)
			return
		}
		other++
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &other
}

func upgradeServices(srv *httptest.Server, exe string) *Services {
	return &Services{Upgrader: &upgrade.Upgrader{
		Base: srv.URL + "/releases/latest/download", Current: "1.0.0", HTTP: srv.Client(),
		Executable: func() (string, error) { return exe, nil },
	}}
}

func TestUpgradeCheckReportsBothVersions(t *testing.T) {
	srv, other := latestServer(t)
	svc := upgradeServices(srv, filepath.Join(t.TempDir(), "smrit"))
	code, out, errs := runCmd(t, svc, "upgrade", "--check")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["current"] != "1.0.0" || got["latest"] != "9.8.7" || got["up_to_date"] != false || got["upgraded"] != false {
		t.Fatalf("%v: %s", err, out)
	}
	code, out, _ = runCmd(t, svc, "--json=false", "upgrade", "--check")
	if code != ExitOK || !strings.Contains(out, "1.0.0") || !strings.Contains(out, "9.8.7") || strings.Contains(out, " - ") {
		t.Fatalf("text: %q", out)
	}
	if *other != 0 {
		t.Fatalf("--check downloaded %d files", *other)
	}
}

func TestUpgradeInsideAPackageManagerExits4(t *testing.T) {
	srv, other := latestServer(t)
	exe := filepath.Join(t.TempDir(), "opt", "homebrew", "bin", "smrit")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCmd(t, upgradeServices(srv, exe), "upgrade")
	if code != ExitRefused || out != "" || !strings.Contains(errs, "package manager") || strings.Count(strings.TrimSpace(errs), "\n") != 0 {
		t.Fatalf("exit %d: %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" || *other != 0 {
		t.Fatal("the binary changed or something was downloaded")
	}
}

func TestUpgradeUnavailableExits5(t *testing.T) {
	srv, _ := latestServer(t)
	svc := upgradeServices(srv, filepath.Join(t.TempDir(), "smrit"))
	svc.Upgrader.Base = srv.URL + "/gone/download"
	if code, _, errs := runCmd(t, svc, "upgrade", "--check"); code != ExitUnavailable || errs == "" {
		t.Fatalf("exit %d: %q", code, errs)
	}
}

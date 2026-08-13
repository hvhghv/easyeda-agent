package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/easyeda-agent/internal/selfupdate"
	"github.com/zhoushoujianwork/easyeda-agent/internal/version"
)

func TestCheckCLIVerdicts(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	cases := []struct {
		name, cur, target, want string
		force                   bool
	}{
		{name: "behind", cur: "v0.25.1", target: "0.26.0", want: "behind"},
		{name: "current", cur: "v0.26.0", target: "0.26.0", want: "up-to-date"},
		{name: "ahead of a pinned older release", cur: "v0.27.0", target: "0.26.0", want: "ahead"},
		{name: "dev build", cur: "v0.25.1-19-gabc-dirty", target: "0.26.0", want: "skipped"},
		{name: "dev build with --force", cur: "dev", target: "0.26.0", want: "behind", force: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			version.Version = c.cur
			got := checkCLI(c.target, c.force)
			if got.Status != c.want {
				t.Errorf("checkCLI(%q→%q, force=%v)=%q want %q", c.cur, c.target, c.force, got.Status, c.want)
			}
		})
	}
}

func TestCheckSkillsReadsVersionMarkers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude", "skills", "easyeda-agent")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, ".version"), []byte("0.25.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// codex dir deliberately absent.

	rows := checkSkills("0.26.0", nil)
	byClient := map[string]updateSkillRow{}
	for _, r := range rows {
		byClient[r.Client] = r
	}
	if got := byClient["claude"].Status; got != "behind" {
		t.Errorf("claude status=%q want behind", got)
	}
	if got := byClient["codex"].Status; got != "not-installed" {
		t.Errorf("codex status=%q want not-installed", got)
	}
	// A client filter must narrow the report.
	if rows := checkSkills("0.26.0", []string{"claude"}); len(rows) != 1 || rows[0].Client != "claude" {
		t.Errorf("--client claude should report exactly the claude dir, got %+v", rows)
	}
}

func TestCountBehindCountsConnectorButNotDevSkip(t *testing.T) {
	rep := updateReport{
		CLI:       &selfupdate.CLIOutcome{Status: "skipped"},
		Skills:    []updateSkillRow{{Status: "behind"}, {Status: "current"}, {Status: "not-installed"}},
		Connector: &connectorReport{Status: "behind"},
	}
	// dev-build skip is intentional (not "behind"); a stale connector counts
	// because the user still has to act on it.
	if got := countBehind(rep); got != 2 {
		t.Errorf("countBehind=%d want 2 (skill + connector)", got)
	}
}

func TestUpdateNotesSurfaceConnectorAndDaemonRestart(t *testing.T) {
	rep := updateReport{
		Target:    "0.26.0",
		CLI:       &selfupdate.CLIOutcome{Status: "updated"},
		Connector: &connectorReport{DaemonRunning: true, Status: "behind", Versions: []string{"0.25.1"}},
	}
	notes := strings.Join(updateNotes(rep), "\n")
	if !strings.Contains(notes, "restart") {
		t.Errorf("a replaced binary with a live daemon must tell the user to restart it: %q", notes)
	}
	if !strings.Contains(notes, ".eext") {
		t.Errorf("a stale connector note must point at the .eext re-import: %q", notes)
	}
}

// fakeDaemon serves a /health payload on a real port so probeConnector exercises
// the same scan path the CLI uses.
func fakeDaemon(t *testing.T, payload string) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, payload)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), p
}

func TestProbeConnectorFlagsStaleConnector(t *testing.T) {
	host, port := fakeDaemon(t, `{"service":"easyeda-agent","version":"v0.26.0","status":"ok",
	  "windows":[{"windowId":"w1","connectorVersion":"0.25.1"},{"windowId":"w2","connectorVersion":"0.26.0"}]}`)
	cfg := &appConfig{host: host, ports: fmt.Sprintf("%d-%d", port, port)}

	rep := probeConnector(cfg, "0.26.0")
	if !rep.DaemonRunning {
		t.Fatal("daemon should be detected")
	}
	if rep.Status != "behind" {
		t.Errorf("status=%q want behind (one window runs 0.25.1)", rep.Status)
	}
	if rep.DaemonVersion != "v0.26.0" || rep.Windows != 2 {
		t.Errorf("unexpected daemon report: %+v", rep)
	}
}

func TestProbeConnectorNoDaemonIsNotAnError(t *testing.T) {
	// A closed port range: probing must degrade to "no-daemon", never fail.
	cfg := &appConfig{host: "127.0.0.1", ports: "1-1"}
	rep := probeConnector(cfg, "0.26.0")
	if rep.Status != "no-daemon" || rep.DaemonRunning {
		t.Errorf("unexpected report with no daemon: %+v", rep)
	}
}

func TestUpdateCheckExitCodeGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude", "skills", "easyeda-agent")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, ".version"), []byte("0.25.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --version pins the target so the check stays offline; ports 1-1 has no daemon.
	args := []string{"update", "--check", "--json", "--version", "0.99.0", "--ports", "1-1"}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("--check without --exit-code must exit 0, got %d: %s", code, stderr.String())
	}
	var rep updateReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("parse report: %v\n%s", err, stdout.String())
	}
	if rep.Mode != "check" || rep.Target != "0.99.0" {
		t.Fatalf("unexpected report head: %+v", rep)
	}
	if rep.Behind == 0 {
		t.Fatalf("a 0.25.1 skill dir is behind 0.99.0, report says nothing is: %+v", rep)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run(append(args, "--exit-code"), &stdout, &stderr)
	if code != exitCodeUpdatesAvailable {
		t.Fatalf("--check --exit-code should exit %d when behind, got %d: %s",
			exitCodeUpdatesAvailable, code, stderr.String())
	}
	// The verdict must not print a bogus "exit status 10" error line.
	if strings.Contains(stderr.String(), "exit status") {
		t.Errorf("exit-code verdict leaked an error message: %q", stderr.String())
	}
}

func TestUpdateRejectsConflictingScopeFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"update", "--cli-only", "--skill-only"}, &stdout, &stderr); code != 1 {
		t.Fatalf("mutually exclusive flags must fail, got exit %d", code)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr should explain the conflict: %q", stderr.String())
	}
}

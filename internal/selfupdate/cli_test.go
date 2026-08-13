package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}:  "easyeda_darwin_arm64",
		{"darwin", "amd64"}:  "easyeda_darwin_amd64",
		{"linux", "amd64"}:   "easyeda_linux_amd64",
		{"linux", "arm64"}:   "easyeda_linux_arm64",
		{"windows", "amd64"}: "easyeda_windows_amd64.exe",
	}
	for in, want := range cases {
		got, err := AssetName(in[0], in[1])
		if err != nil {
			t.Fatalf("AssetName(%q,%q): %v", in[0], in[1], err)
		}
		if got != want {
			t.Errorf("AssetName(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
	// Platforms `make release` doesn't publish must fail loudly rather than
	// 404 halfway through an update.
	for _, in := range [][2]string{{"windows", "arm64"}, {"linux", "riscv64"}, {"plan9", "amd64"}} {
		if _, err := AssetName(in[0], in[1]); err == nil {
			t.Errorf("AssetName(%q,%q) should error (no published asset)", in[0], in[1])
		}
	}
}

// updateServer serves one release binary (+ optional checksums.txt) and points
// the package's URL builders at itself.
func updateServer(t *testing.T, version, body string, withChecksums bool) {
	t.Helper()
	asset, err := AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no release asset for this platform: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bin/" + version:
			fmt.Fprint(w, body)
		case "/checksums/" + version:
			if !withChecksums {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, "%s  %s\n%s  skills.tar.gz\n", hex.EncodeToString(sum[:]), asset, "deadbeef")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	oldBin, oldSums := binaryURL, checksumsURL
	binaryURL = func(v, _ string) string { return srv.URL + "/bin/" + v }
	checksumsURL = func(v string) string { return srv.URL + "/checksums/" + v }
	t.Cleanup(func() { binaryURL, checksumsURL = oldBin, oldSums })
}

// stubVerify replaces the run-the-binary check (the downloaded "binary" is a
// text fixture, so it cannot actually be executed).
func stubVerify(t *testing.T, err error) {
	t.Helper()
	old := verifyBinary
	verifyBinary = func(context.Context, string, string) error { return err }
	t.Cleanup(func() { verifyBinary = old })
}

// installedBinary writes a fake current binary and returns its path.
func installedBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "easyeda")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpdateCLIReplacesBinaryAndVerifiesChecksum(t *testing.T) {
	updateServer(t, "0.26.0", "NEW-BINARY", true)
	stubVerify(t, nil)
	path := installedBinary(t, "OLD-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion:  "0.26.0",
		CurrentVersion: "v0.25.1",
		Path:           path,
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCLI: %v", err)
	}
	if out.Status != "updated" {
		t.Fatalf("status=%q reason=%q want updated", out.Status, out.Reason)
	}
	if out.Checksum != "verified" {
		t.Errorf("checksum=%q want verified", out.Checksum)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Errorf("binary content = %q, want the downloaded one", got)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("replacement is not executable (mode %v)", fi.Mode())
	}
	// The temp download must not survive the swap.
	assertNoLeftovers(t, filepath.Dir(path))
}

func TestUpdateCLIWithoutChecksumsStillUpdates(t *testing.T) {
	// Releases published before checksums.txt existed must stay upgradable.
	updateServer(t, "0.26.0", "NEW-BINARY", false)
	stubVerify(t, nil)
	path := installedBinary(t, "OLD-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.26.0", CurrentVersion: "v0.25.1", Path: path,
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCLI: %v", err)
	}
	if out.Status != "updated" || out.Checksum != "unavailable" {
		t.Fatalf("status=%q checksum=%q want updated/unavailable", out.Status, out.Checksum)
	}
}

func TestUpdateCLIChecksumMismatchKeepsOldBinary(t *testing.T) {
	updateServer(t, "0.26.0", "NEW-BINARY", true)
	// Serve a different body than the one the checksum was computed over by
	// re-pointing binaryURL only.
	old := binaryURL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "TAMPERED")
	}))
	defer srv.Close()
	binaryURL = func(string, string) string { return srv.URL }
	defer func() { binaryURL = old }()
	stubVerify(t, nil)
	path := installedBinary(t, "OLD-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.26.0", CurrentVersion: "v0.25.1", Path: path,
	}, nil)
	if err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	if out.Status != "error" {
		t.Errorf("status=%q want error", out.Status)
	}
	if got, _ := os.ReadFile(path); string(got) != "OLD-BINARY" {
		t.Errorf("binary was replaced despite a checksum mismatch: %q", got)
	}
	assertNoLeftovers(t, filepath.Dir(path))
}

func TestUpdateCLIUnrunnableDownloadKeepsOldBinary(t *testing.T) {
	updateServer(t, "0.26.0", "NEW-BINARY", false)
	stubVerify(t, fmt.Errorf("exec format error"))
	path := installedBinary(t, "OLD-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.26.0", CurrentVersion: "v0.25.1", Path: path,
	}, nil)
	if err == nil {
		t.Fatal("a binary that won't run must fail the update")
	}
	if out.Status != "error" {
		t.Errorf("status=%q want error", out.Status)
	}
	if got, _ := os.ReadFile(path); string(got) != "OLD-BINARY" {
		t.Errorf("binary replaced despite failing verification: %q", got)
	}
	assertNoLeftovers(t, filepath.Dir(path))
}

func TestUpdateCLIRefusesToOverwriteDevBuild(t *testing.T) {
	updateServer(t, "0.26.0", "NEW-BINARY", true)
	stubVerify(t, nil)
	path := installedBinary(t, "DEV-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.26.0", CurrentVersion: "v0.25.1-19-gabc-dirty", Path: path,
	}, nil)
	if err != nil {
		t.Fatalf("a dev build is a benign skip, not an error: %v", err)
	}
	if out.Status != "skipped" {
		t.Fatalf("status=%q want skipped", out.Status)
	}
	if got, _ := os.ReadFile(path); string(got) != "DEV-BINARY" {
		t.Errorf("dev build was overwritten: %q", got)
	}
}

func TestUpdateCLIForceOverwritesDevBuild(t *testing.T) {
	updateServer(t, "0.26.0", "NEW-BINARY", true)
	stubVerify(t, nil)
	path := installedBinary(t, "DEV-BINARY")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.26.0", CurrentVersion: "dev", Path: path, Force: true,
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCLI --force: %v", err)
	}
	if out.Status != "updated" {
		t.Fatalf("status=%q want updated", out.Status)
	}
	if got, _ := os.ReadFile(path); string(got) != "NEW-BINARY" {
		t.Errorf("--force did not replace the binary: %q", got)
	}
}

func TestUpdateCLIUpToDateSkipsDownload(t *testing.T) {
	// No server at all: an up-to-date check must not hit the network.
	oldBin := binaryURL
	binaryURL = func(string, string) string {
		t.Error("up-to-date must not download")
		return "http://127.0.0.1:0"
	}
	defer func() { binaryURL = oldBin }()
	path := installedBinary(t, "SAME")

	out, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "0.25.1", CurrentVersion: "v0.25.1", Path: path,
	}, nil)
	if err != nil {
		t.Fatalf("UpdateCLI: %v", err)
	}
	if out.Status != "up-to-date" {
		t.Errorf("status=%q want up-to-date", out.Status)
	}
}

func TestUpdateCLIBadTargetVersion(t *testing.T) {
	if _, err := UpdateCLI(context.Background(), CLIOptions{
		TargetVersion: "latest", CurrentVersion: "v0.25.1", Path: installedBinary(t, "OLD"),
	}, nil); err == nil {
		t.Error("non-semver target must error")
	}
}

func TestFetchChecksumParsesShasumFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "aaa  easyeda_linux_amd64\nbbb  *easyeda_darwin_arm64\nnot-a-line\n")
	}))
	defer srv.Close()
	old := checksumsURL
	checksumsURL = func(string) string { return srv.URL }
	defer func() { checksumsURL = old }()

	got, err := fetchChecksum(context.Background(), "0.26.0", "easyeda_darwin_arm64")
	if err != nil {
		t.Fatalf("fetchChecksum: %v", err)
	}
	if got != "bbb" {
		t.Errorf("checksum=%q want bbb", got)
	}
	if _, err := fetchChecksum(context.Background(), "0.26.0", "easyeda_windows_amd64.exe"); err == nil {
		t.Error("a missing asset entry must error so the caller skips verification")
	}
}

// assertNoLeftovers fails if a partial download survived in the install dir —
// a stray .easyeda-update-* file next to the binary would be confusing at best.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 1 && e.Name()[0] == '.' {
			t.Errorf("leftover temp file in install dir: %s", e.Name())
		}
	}
}

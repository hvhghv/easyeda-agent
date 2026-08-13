package selfupdate

// CLI binary self-update. Complements the skill sync in selfupdate.go: the same
// GitHub release carries the platform binaries (`easyeda_<os>_<arch>`), so
// `easyeda update` can replace the running CLI in place instead of asking the
// user to re-run install.sh.
//
// Deliberate non-goals:
//   - the connector .eext is NOT updated here (sideloads have no in-place
//     update; see the package doc), only reported as stale by the caller;
//   - a dev build (git-describe stamp, "dev") is never overwritten unless the
//     caller forces it — air rebuilds it on the next .go change anyway, and
//     silently replacing it with a release binary makes the dev loop lie.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Endpoint builders, overridable in tests to point at an httptest server.
var (
	binaryURL = func(version, asset string) string {
		return fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", RepoSlug, version, asset)
	}
	checksumsURL = func(version string) string {
		return fmt.Sprintf("https://github.com/%s/releases/download/v%s/checksums.txt", RepoSlug, version)
	}
)

// verifyBinary runs the freshly-downloaded binary and checks it reports the
// expected version — the strongest cheap integrity check we have, and it also
// catches a truncated download or an HTML error page saved as a binary.
// Overridable in tests.
var verifyBinary = func(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("downloaded binary failed to run (%v): %s", err, strings.TrimSpace(string(out)))
	}
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, want) {
		return fmt.Errorf("downloaded binary reports %q, expected version %s", got, want)
	}
	return nil
}

// AssetName maps a Go platform onto the release asset name published by
// `make release`. Returns an error for platforms we don't ship.
func AssetName(goos, goarch string) (string, error) {
	switch goos {
	case "darwin", "linux":
		if goarch == "amd64" || goarch == "arm64" {
			return fmt.Sprintf("easyeda_%s_%s", goos, goarch), nil
		}
	case "windows":
		if goarch == "amd64" {
			return "easyeda_windows_amd64.exe", nil
		}
	}
	return "", fmt.Errorf("no release binary published for %s/%s — build from source (go install ./cmd/easyeda)", goos, goarch)
}

// CurrentBinaryPath returns the absolute, symlink-resolved path of the running
// executable — the file `update` replaces.
func CurrentBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// CLIOptions configures UpdateCLI.
type CLIOptions struct {
	// TargetVersion is the release to install (bare "x.y.z" or "vx.y.z").
	TargetVersion string
	// CurrentVersion is the running CLI's version stamp (may be a dev stamp).
	CurrentVersion string
	// Path overrides the binary to replace (default: the running executable).
	Path string
	// Force overwrites even when the current build is a dev stamp or already
	// at the target version.
	Force bool
}

// CLIOutcome is the result of a CLI binary update attempt.
type CLIOutcome struct {
	Path     string `json:"path"`
	From     string `json:"from"`
	To       string `json:"to"`
	Asset    string `json:"asset,omitempty"`
	Status   string `json:"status"` // updated | up-to-date | skipped | error
	Reason   string `json:"reason,omitempty"`
	Checksum string `json:"checksum,omitempty"` // verified | unavailable
}

// UpdateCLI downloads the target release binary for this platform and atomically
// replaces `path`. It returns a structured outcome plus an error for real
// failures; a benign no-op (already current, dev build) is reported as a status
// with a nil error so callers don't have to special-case it.
//
// The replacement is a same-directory rename, so it is atomic and never leaves a
// half-written binary on PATH. A running daemon keeps its old inode — the caller
// tells the user to restart it.
func UpdateCLI(ctx context.Context, opts CLIOptions, logf func(string, ...any)) (CLIOutcome, error) {
	log := func(format string, a ...any) {
		if logf != nil {
			logf(format, a...)
		}
	}
	target := SemverCore(opts.TargetVersion)
	out := CLIOutcome{From: opts.CurrentVersion, To: target}
	if target == "" {
		out.Status = "error"
		out.Reason = fmt.Sprintf("bad target version %q", opts.TargetVersion)
		return out, fmt.Errorf("update cli: %s", out.Reason)
	}

	path := opts.Path
	if path == "" {
		p, err := CurrentBinaryPath()
		if err != nil {
			out.Status = "error"
			out.Reason = err.Error()
			return out, fmt.Errorf("locate running binary: %w", err)
		}
		path = p
	}
	out.Path = path

	if !opts.Force {
		if !IsCleanRelease(opts.CurrentVersion) {
			out.Status = "skipped"
			out.Reason = fmt.Sprintf("dev build (%s) — refusing to overwrite; use --force to install v%s anyway",
				opts.CurrentVersion, target)
			return out, nil
		}
		if SemverCore(opts.CurrentVersion) == target {
			out.Status = "up-to-date"
			return out, nil
		}
	}

	asset, err := AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		out.Status = "error"
		out.Reason = err.Error()
		return out, err
	}
	out.Asset = asset

	dir := filepath.Dir(path)
	if err := checkWritable(dir); err != nil {
		out.Status = "error"
		out.Reason = err.Error()
		return out, fmt.Errorf("cannot replace %s: %w", path, err)
	}

	mode := os.FileMode(0o755)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm() | 0o111
	}

	log("update: downloading %s v%s", asset, target)
	tmp, sum, err := downloadTo(ctx, binaryURL(target, asset), dir, mode)
	if err != nil {
		out.Status = "error"
		out.Reason = err.Error()
		return out, fmt.Errorf("download %s v%s: %w", asset, target, err)
	}
	defer os.Remove(tmp) // no-op once the rename succeeds

	// Checksum verification is best-effort: checksums.txt only exists on
	// releases built after it was added, so a missing file must not block an
	// upgrade from an older release.
	switch want, err := fetchChecksum(ctx, target, asset); {
	case err != nil:
		out.Checksum = "unavailable"
		log("update: checksum unavailable (%v) — falling back to a run-and-verify check", err)
	case !strings.EqualFold(want, sum):
		out.Status = "error"
		out.Reason = fmt.Sprintf("checksum mismatch: want %s, got %s", want, sum)
		return out, fmt.Errorf("checksum mismatch for %s v%s (want %s, got %s)", asset, target, want, sum)
	default:
		out.Checksum = "verified"
		log("update: sha256 verified")
	}

	if err := verifyBinary(ctx, tmp, target); err != nil {
		out.Status = "error"
		out.Reason = err.Error()
		return out, fmt.Errorf("verify downloaded binary: %w", err)
	}

	if err := replaceBinary(tmp, path); err != nil {
		out.Status = "error"
		out.Reason = err.Error()
		return out, fmt.Errorf("install %s: %w", path, err)
	}
	out.Status = "updated"
	log("update: %s → v%s (%s)", displayVersion(opts.CurrentVersion), target, path)
	return out, nil
}

func displayVersion(v string) string {
	if v == "" {
		return "?"
	}
	return v
}

// checkWritable reports whether we can create (and thus rename into) dir. The
// common failure is /usr/local/bin owned by root, so the message says so.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".easyeda-update-probe-*")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("%s is not writable — re-run with sudo (sudo easyeda update) "+
				"or install into a user-owned dir (~/.local/bin)", dir)
		}
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// downloadTo streams url into a temp file inside dir (same filesystem as the
// target, so the later rename is atomic) and returns its path plus the sha256.
func downloadTo(ctx context.Context, url, dir string, mode os.FileMode) (path, sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".easyeda-update-*")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	h := sha256.New()
	// 256 MB ceiling: far above any real binary, low enough that a runaway
	// response can't fill the disk.
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 256<<20)); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), nil
}

// fetchChecksum pulls checksums.txt for the release and returns the sha256 hex
// recorded for asset. Errors (including "no such asset published") are the
// caller's cue to skip checksum verification.
func fetchChecksum(ctx context.Context, version, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumsURL(version), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksums.txt: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		// sha256sum/shasum format: "<hex>  <name>" (also tolerate "*name").
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", asset)
}

// replaceBinary moves tmp onto dst. On Unix a rename over a running executable
// is fine (the running process keeps the old inode). Windows refuses to replace
// a file that is open for execution, so the old one is moved aside first.
func replaceBinary(tmp, dst string) error {
	if runtime.GOOS == "windows" {
		old := dst + ".old"
		_ = os.Remove(old)
		if err := os.Rename(dst, old); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Rename(old, dst) // put it back
			return err
		}
		_ = os.Remove(old) // fails while still running; harmless leftover
		return nil
	}
	return os.Rename(tmp, dst)
}

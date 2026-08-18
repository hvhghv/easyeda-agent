package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStripArtifactNesting: the outputDir sent to the daemon must never point
// INSIDE a .easyeda/artifacts tree — a drifted cwd used to make the daemon
// nest .easyeda/artifacts/.easyeda/artifacts/… one level deeper per call.
// 1-level and 3-level nested inputs must normalize to the SAME target.
func TestStripArtifactNesting(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Join(sep+"home", "me", "proj")
	one := filepath.Join(root, ".easyeda", "artifacts")
	three := filepath.Join(root,
		".easyeda", "artifacts", ".easyeda", "artifacts", ".easyeda", "artifacts")

	if got := stripArtifactNesting(one); got != root {
		t.Errorf("1-level nesting: got %q, want %q", got, root)
	}
	if got := stripArtifactNesting(three); got != root {
		t.Errorf("3-level nesting: got %q, want %q", got, root)
	}
	// A cwd BELOW the artifact dir (e.g. inside a dated subfolder) also snaps back.
	deepInside := filepath.Join(one, "20260801", "snaps")
	if got := stripArtifactNesting(deepInside); got != root {
		t.Errorf("cwd below artifacts: got %q, want %q", got, root)
	}
	// A normal path is untouched (modulo Clean).
	if got := stripArtifactNesting(root); got != root {
		t.Errorf("normal path changed: got %q, want %q", got, root)
	}
	// A lone .easyeda (no artifacts child segment) is NOT stripped.
	cfgDir := filepath.Join(root, ".easyeda")
	if got := stripArtifactNesting(cfgDir); got != cfgDir {
		t.Errorf(".easyeda without artifacts stripped: got %q, want %q", got, cfgDir)
	}
	// Degenerate: the pair at the filesystem root must not panic or return "".
	if got := stripArtifactNesting(filepath.Join(sep+".easyeda", "artifacts")); got != sep {
		t.Errorf("rooted pair: got %q, want %q", got, sep)
	}
}

// TestResolveOutputDir: after stripping, the dir anchors to the nearest
// enclosing project root (.git / go.mod marker walk-up), falling back to the
// stripped cwd when no marker exists.
func TestResolveOutputDir(t *testing.T) {
	sep := string(filepath.Separator)
	projRoot := filepath.Join(sep+"repo", "mono")
	isRoot := func(d string) bool { return d == projRoot }

	// cwd in a subdir → anchored up to the project root.
	sub := filepath.Join(projRoot, "internal", "app")
	if got := resolveOutputDir(sub, isRoot); got != projRoot {
		t.Errorf("subdir: got %q, want %q", got, projRoot)
	}
	// cwd inside a nested artifact tree → stripped first, then anchored: the
	// nested 1-level and 3-level cases land on the SAME directory.
	one := filepath.Join(projRoot, ".easyeda", "artifacts")
	three := filepath.Join(projRoot, ".easyeda", "artifacts", ".easyeda", "artifacts", ".easyeda", "artifacts")
	g1, g3 := resolveOutputDir(one, isRoot), resolveOutputDir(three, isRoot)
	if g1 != projRoot || g3 != projRoot || g1 != g3 {
		t.Errorf("nested: got %q / %q, want both %q", g1, g3, projRoot)
	}
	// No marker anywhere → fall back to the stripped cwd unchanged.
	noRoot := func(string) bool { return false }
	elsewhere := filepath.Join(sep+"tmp", "scratch")
	if got := resolveOutputDir(elsewhere, noRoot); got != elsewhere {
		t.Errorf("no marker: got %q, want %q", got, elsewhere)
	}
	if got := resolveOutputDir(three, noRoot); got != projRoot {
		t.Errorf("no marker but nested: got %q, want stripped %q", got, projRoot)
	}
}

// TestArtifactOutputDirLive: run against the real repo cwd — this test file
// lives under the module, so the marker walk-up must find a root that carries
// .git or go.mod, and the result must never contain a .easyeda/artifacts pair.
func TestArtifactOutputDirLive(t *testing.T) {
	dir, ok := artifactOutputDir()
	if !ok {
		t.Fatal("artifactOutputDir() not ok")
	}
	if !dirHasProjectMarker(dir) {
		t.Errorf("resolved dir %q has no project marker", dir)
	}
	if strings.Contains(dir, filepath.Join(".easyeda", "artifacts")) {
		t.Errorf("resolved dir %q still inside an artifact tree", dir)
	}
}

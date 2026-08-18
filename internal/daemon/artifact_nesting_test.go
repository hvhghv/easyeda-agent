package daemon

import (
	"path/filepath"
	"testing"
)

// TestArtifactDirIdempotent: artifactDir must be IDEMPOTENT over its own
// output — an outputDir already pointing at (or nested inside) a
// .easyeda/artifacts tree must normalize back to the project root before the
// Join, or every dispatch cycle grows the tree one level deeper
// (.easyeda/artifacts/.easyeda/artifacts/… — observed 4 levels in the wild).
func TestArtifactDirIdempotent(t *testing.T) {
	s := &Server{}
	sep := string(filepath.Separator)
	root := filepath.Join(sep+"home", "me", "proj")
	want := filepath.Join(root, ".easyeda", "artifacts")

	// Clean cwd → unchanged behavior.
	if got := s.artifactDir(root); got != want {
		t.Errorf("clean: got %q, want %q", got, want)
	}
	// 1-level nested input (= artifactDir's own previous output) → same target.
	if got := s.artifactDir(want); got != want {
		t.Errorf("1-level nested: got %q, want %q", got, want)
	}
	// 3-level nested input → still the same target.
	three := filepath.Join(root,
		".easyeda", "artifacts", ".easyeda", "artifacts", ".easyeda", "artifacts")
	if got := s.artifactDir(three); got != want {
		t.Errorf("3-level nested: got %q, want %q", got, want)
	}
}

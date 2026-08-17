package app

// idlist.go — one shared parser for every id-list CLI flag (--ids).
// History (issue #109): `fill delete` / `pour-delete` / `region delete` /
// `pcb delete` took a JSON array payload while `track-delete` / `via-delete`
// took CSV — the same agent flip-flopped between the two formats mid-session
// and got it wrong both ways. The format is now CSV, everywhere, full stop:
// JSON array strings are rejected with a pointed error instead of being
// half-parsed into garbage ids.

import (
	"fmt"
	"strings"
)

// parseIDList normalizes an --ids flag value into a []string of primitive ids.
// The one accepted form is CSV:
//
//	id1,id2   (spaces around commas ok, empty items dropped)
//
// A value that looks like a JSON array (leading '[') is rejected explicitly —
// the legacy JSON-array format was removed, and splitting it as CSV would
// produce garbage ids like `["id1"`.
//
// Duplicates are dropped (first occurrence wins): the platform delete API
// silently rejects the ENTIRE batch when the list contains a repeated id
// (live 2026-08-17, P2 — a stitched-together delete prescription re-listed one
// id and every delete in the batch became a no-op that still returned ok).
func parseIDList(s string) ([]string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, fmt.Errorf("empty id list — pass CSV: id1,id2")
	}
	if strings.HasPrefix(t, "[") {
		return nil, fmt.Errorf("--ids no longer accepts a JSON array — pass CSV: id1,id2")
	}
	var out []string
	seen := map[string]bool{}
	for p := range strings.SplitSeq(t, ",") {
		if id := strings.TrimSpace(p); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ids found in %q — pass CSV: id1,id2", s)
	}
	return out, nil
}

// uniqueIDs drops duplicate ids in place-order (first wins) — for delete
// batches built programmatically (deep-sweep, dedupe prescriptions), which hit
// the same silently-rejecting platform behavior as CSV input.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

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
func parseIDList(s string) ([]string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, fmt.Errorf("empty id list — pass CSV: id1,id2")
	}
	if strings.HasPrefix(t, "[") {
		return nil, fmt.Errorf("--ids no longer accepts a JSON array — pass CSV: id1,id2")
	}
	var out []string
	for _, p := range strings.Split(t, ",") {
		if id := strings.TrimSpace(p); id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ids found in %q — pass CSV: id1,id2", s)
	}
	return out, nil
}

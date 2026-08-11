package app

// cmd_sch_modify_patch.go — patch assembly for `sch modify`.
//
// `sch place` takes --x/--y/--rotation/--designator as flags, so agents
// instinctively try the same on `sch modify` — which historically only took a
// --patch JSON blob. modify now accepts both sources: quick flags for the
// common geometry/designator tweaks, --patch for everything else
// (customAttributes, BOM flags, ...). On a key collision the explicit flag
// wins and the override is reported, so a stale --patch value never silently
// beats what the caller typed as a flag.

import (
	"encoding/json"
	"fmt"
	"sort"
)

// buildModifyPatch merges quick-flag overrides into the (optional) --patch
// JSON object. overrides must contain ONLY flags the caller explicitly set
// (cobra Changed — so an unset --x never writes a spurious 0). Returns the
// merged patch and the sorted list of patch keys the flags overrode.
// Pure function — unit-testable without a live editor.
func buildModifyPatch(patchJSON string, overrides map[string]any) (map[string]any, []string, error) {
	if patchJSON == "" && len(overrides) == 0 {
		return nil, nil, fmt.Errorf("nothing to modify — pass --x/--y/--rotation/--designator and/or --patch '{...}'")
	}
	patch := map[string]any{}
	if patchJSON != "" {
		if err := json.Unmarshal([]byte(patchJSON), &patch); err != nil {
			return nil, nil, fmt.Errorf("invalid --patch json: %w", err)
		}
	}
	var overridden []string
	for k, v := range overrides {
		if _, clash := patch[k]; clash && patchJSON != "" {
			overridden = append(overridden, k)
		}
		patch[k] = v
	}
	if len(patch) == 0 {
		// e.g. --patch '{}' with no flags: nothing would be sent.
		return nil, nil, fmt.Errorf("empty patch — pass --x/--y/--rotation/--designator and/or a non-empty --patch")
	}
	sort.Strings(overridden)
	return patch, overridden, nil
}

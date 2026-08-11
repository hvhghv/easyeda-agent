package app

// cmd_sch_pin_target.go — shared pin-target resolution for `sch connect`.
//
// `sch autoconnect` / `sch disconnect` address pins as `--pin DESIGNATOR:PIN`,
// and agents reach for the same spelling on `sch connect` — which historically
// only took raw `--x/--y` coordinates (a real misuse trap: same family, different
// parameter style). `sch connect` now accepts BOTH, mutually exclusive:
//
//	--pin U1:5      designator:pin — resolved to the pin coordinate via the
//	                same scene machinery autoconnect uses (components.list
//	                --include-pins → buildScene → resolvePinCoord), so off-page
//	                / ambiguous / typo'd refs get autoconnect's diagnostics
//	--x / --y       raw coordinates — legitimate on its own (primitives without
//	                a designator, exact-coordinate workflows), not a legacy path

import "fmt"

// validatePinTarget enforces the --pin XOR --x/--y contract for `sch connect`.
// pinSet reports --pin non-empty; xSet/ySet report the flags EXPLICITLY passed
// (cobra Changed, so a literal --y 0 counts). Pure function — unit-testable
// without a live editor.
func validatePinTarget(pinSet, xSet, ySet bool) error {
	switch {
	case pinSet && (xSet || ySet):
		return fmt.Errorf("--pin and --x/--y are mutually exclusive — pass ONE pin locator (--pin resolves the coordinate itself)")
	case pinSet:
		return nil
	case xSet && ySet:
		return nil
	case xSet || ySet:
		return fmt.Errorf("--x and --y must be given together (or use --pin DESIGNATOR:PIN)")
	default:
		return fmt.Errorf("pass --pin DESIGNATOR:PIN or both --x and --y")
	}
}

// resolveSchPinXY resolves a DESIGNATOR:PIN reference to the pin's coordinate
// on the active schematic page, reusing autoconnect's scene builder so the
// error surface (off-page hint, ambiguity fan-out hint, pin-typo hint) is
// identical across the command family.
func resolveSchPinXY(cfg *appConfig, window, ref string) (x, y float64, err error) {
	res, err := requestAction(cfg, "schematic.components.list", window, map[string]any{
		"includeBBox": true,
		"includePins": true,
	})
	if err != nil {
		return 0, 0, err
	}
	scene := buildScene(res.Result)
	pin, err := resolvePinCoord(scene, ref)
	if err != nil {
		return 0, 0, err
	}
	return pin.X, pin.Y, nil
}

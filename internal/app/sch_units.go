package app

// EasyEDA Pro schematic geometry is expressed in 0.01-inch canvas units.
// Keep conversion at the CLI boundary: planners and connector payloads stay in
// native canvas units, while user-facing distance flags and reports use mm.
const schematicUnitMM = 0.254

func schematicUnitsToMM(v float64) float64 {
	return v * schematicUnitMM
}

func mmToSchematicUnits(v float64) float64 {
	return v / schematicUnitMM
}

package store

// SubtractSoftware returns the names in want that are not in have,
// keeping want's order. Both lists are expected to be normalized.
func SubtractSoftware(want, have []string) []string {
	got := make(map[string]bool, len(have))
	for _, n := range have {
		got[n] = true
	}
	out := []string{}
	for _, n := range want {
		if !got[n] {
			out = append(out, n)
		}
	}
	return out
}

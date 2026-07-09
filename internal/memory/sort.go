package memory

import "sort"

// sortHitsDesc orders hits by descending score.
func sortHitsDesc(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
}

// sortMemsByWeightAsc orders memories by ascending weight (lowest first), so an
// eviction can drop the front of the slice.
func sortMemsByWeightAsc(mems []Memory, weight func(Memory) float64) {
	sort.SliceStable(mems, func(i, j int) bool { return weight(mems[i]) < weight(mems[j]) })
}

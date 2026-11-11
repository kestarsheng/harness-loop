package rrf

import "sort"

func StableSort(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.RRFScore != b.RRFScore {
			return a.RRFScore > b.RRFScore
		}
		if a.Entity.FilePath != b.Entity.FilePath {
			return a.Entity.FilePath < b.Entity.FilePath
		}
		if a.Entity.StartLine != b.Entity.StartLine {
			return a.Entity.StartLine < b.Entity.StartLine
		}
		return a.Entity.ID < b.Entity.ID
	})
}

func DedupOnly(routes ...Route) []Hit {
	hits := dedup(routes)
	StableSort(hits)
	return hits
}

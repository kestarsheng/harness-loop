package rrf

const DefaultK = 60

type Entity struct {
	ID        string
	FilePath  string
	StartLine int
}

type RankedItem struct {
	Entity
	RawScore float64
}

type Route struct {
	Name  string
	Items []RankedItem
}

type Hit struct {
	Entity
	RRFScore float64
	RawScore float64
	Ranks    []int
	Sources  []string
}

type Fusion struct {
	K int
}

func New(k int) Fusion {
	if k <= 0 {
		k = DefaultK
	}
	return Fusion{K: k}
}

func (f Fusion) Score(ranks []int) float64 {
	k := f.K
	if k <= 0 {
		k = DefaultK
	}
	total := 0.0
	for _, rank := range ranks {
		if rank < 0 {
			continue
		}
		total += 1.0 / float64(k+rank+1)
	}
	return total
}

func (f Fusion) MergeRoutes(routes ...Route) []Hit {
	merged := dedup(routes)
	for i := range merged {
		merged[i].RRFScore = f.Score(merged[i].Ranks)
	}
	StableSort(merged)
	return merged
}

func (f Fusion) MergeKeys(lists ...[]string) []Hit {
	routes := make([]Route, 0, len(lists))
	for i, list := range lists {
		items := make([]RankedItem, 0, len(list))
		for _, key := range list {
			items = append(items, RankedItem{Entity: Entity{ID: key}})
		}
		routes = append(routes, Route{Name: sourceName(i), Items: items})
	}
	return f.MergeRoutes(routes...)
}

func sourceName(i int) string {
	switch i {
	case 0:
		return "fulltext"
	case 1:
		return "vector"
	default:
		return "route"
	}
}

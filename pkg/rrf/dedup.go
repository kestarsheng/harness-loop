package rrf

import "fmt"

func entityKey(e Entity) string {
	if e.ID != "" {
		return e.ID
	}
	return fmt.Sprintf("%s#%d", e.FilePath, e.StartLine)
}

func dedup(routes []Route) []Hit {
	type slot struct {
		hit Hit
	}
	order := make([]string, 0)
	slots := make(map[string]*slot)

	for routeIndex, route := range routes {
		for rank, item := range route.Items {
			key := entityKey(item.Entity)
			if key == "" {
				continue
			}
			s, ok := slots[key]
			if !ok {
				s = &slot{hit: Hit{Entity: item.Entity, Ranks: make([]int, len(routes)), RawScore: item.RawScore}}
				for i := range s.hit.Ranks {
					s.hit.Ranks[i] = -1
				}
				slots[key] = s
				order = append(order, key)
			}
			if s.hit.Ranks[routeIndex] < 0 {
				s.hit.Ranks[routeIndex] = rank
				s.hit.Sources = append(s.hit.Sources, route.Name)
			}
			s.hit.RawScore += item.RawScore
			if s.hit.Entity.FilePath == "" && item.FilePath != "" {
				s.hit.Entity.FilePath = item.FilePath
			}
			if s.hit.Entity.StartLine == 0 && item.StartLine != 0 {
				s.hit.Entity.StartLine = item.StartLine
			}
		}
	}

	out := make([]Hit, 0, len(order))
	for _, key := range order {
		out = append(out, slots[key].hit)
	}
	return out
}

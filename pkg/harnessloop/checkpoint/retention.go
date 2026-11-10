package checkpoint

import "context"

func RetentionPlan(cps []*Checkpoint, keepRecent int) (keep, remove []string) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	keepSet := make(map[string]bool, len(cps))

	for i, cp := range cps {
		if i < keepRecent {
			keepSet[cp.CheckpointID] = true
		}
	}

	seenStage := make(map[string]bool, 4)
	for _, cp := range cps {
		if !seenStage[cp.Stage] {
			seenStage[cp.Stage] = true
			keepSet[cp.CheckpointID] = true
		}
	}

	for _, cp := range cps {
		if keepSet[cp.CheckpointID] {
			keep = append(keep, cp.CheckpointID)
		} else {
			remove = append(remove, cp.CheckpointID)
		}
	}
	return keep, remove
}

func (s *MemoryStore) ApplyRetention(ctx context.Context, taskID string, keepRecent int) (removed []string, err error) {
	list, err := s.List(ctx, taskID, 0)
	if err != nil {
		return nil, err
	}
	_, remove := RetentionPlan(list, keepRecent)
	for _, id := range remove {
		if err := s.Delete(ctx, id); err != nil {
			return removed, err
		}
		removed = append(removed, id)
	}
	return removed, nil
}

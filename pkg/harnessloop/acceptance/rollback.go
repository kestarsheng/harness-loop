package acceptance

func DefaultRollback(stage string) string {
	switch stage {
	case "a":
		return "a"
	case "b":
		return "a"
	case "c":
		return "b"
	case "d":
		return "c"
	default:
		return "a"
	}
}

var allowedRollbackTargets = map[string]map[string]bool{
	"a": {"a": true},
	"b": {"a": true},
	"c": {"b": true},
	"d": {"b": true, "c": true},
}

func AllowedRollback(stage, target string) bool {
	targets, ok := allowedRollbackTargets[stage]
	if !ok {
		return false
	}
	return targets[target]
}

func RollbackFor(stage string, failed []FailedCriterion, confidence float64, advice string) (string, bool) {
	target := ruleTarget(stage, failed, confidence)

	if advice == "" {
		return target, false
	}
	if advice == target {
		return target, false
	}
	return target, true
}

func ruleTarget(stage string, failed []FailedCriterion, confidence float64) string {
	base := DefaultRollback(stage)
	if stage != "d" {
		return base
	}
	hasPlan := false
	hasRequirement := false
	allImplOrTest := len(failed) > 0
	hasUnknown := false
	for _, f := range failed {
		switch f.Category {
		case "plan_gap":
			hasPlan = true
		case "requirement_gap":
			hasRequirement = true
		case "impl_gap", "test_gap":
		case "unknown", "":
			hasUnknown = true
			allImplOrTest = false
		default:
			hasUnknown = true
			allImplOrTest = false
		}
	}
	if hasPlan || hasRequirement {
		return "b"
	}
	if allImplOrTest {
		return "c"
	}
	if hasUnknown && confidence < 0.5 {
		return "c"
	}
	return base
}

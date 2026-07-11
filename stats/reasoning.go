package stats

import "strings"

const unspecifiedReasoningEffort = "unspecified"

// ClassifyReasoningEffort returns the requested effort or derives one from a
// thinking budget. Explicit values are normalized but otherwise preserved.
func ClassifyReasoningEffort(explicit string, budget *int) string {
	if effort := strings.ToLower(strings.TrimSpace(explicit)); effort != "" {
		return effort
	}
	if budget == nil {
		return unspecifiedReasoningEffort
	}
	if *budget >= 16000 {
		return "high"
	}
	if *budget >= 8000 {
		return "medium"
	}
	return "low"
}

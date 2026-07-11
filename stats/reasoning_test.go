package stats

import "testing"

func TestClassifyReasoningEffort(t *testing.T) {
	zero := 0
	low := 7999
	medium := 8000
	mediumMax := 15999
	high := 16000

	tests := []struct {
		name     string
		explicit string
		budget   *int
		want     string
	}{
		{name: "unspecified", want: "unspecified"},
		{name: "explicit normalized", explicit: "  XHIGH ", want: "xhigh"},
		{name: "explicit minimal preserved", explicit: "minimal", want: "minimal"},
		{name: "explicit none preserved", explicit: "none", want: "none"},
		{name: "explicit wins over budget", explicit: "HIGH", budget: &zero, want: "high"},
		{name: "zero budget", budget: &zero, want: "low"},
		{name: "below medium", budget: &low, want: "low"},
		{name: "medium boundary", budget: &medium, want: "medium"},
		{name: "below high", budget: &mediumMax, want: "medium"},
		{name: "high boundary", budget: &high, want: "high"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyReasoningEffort(tt.explicit, tt.budget); got != tt.want {
				t.Fatalf("ClassifyReasoningEffort(%q, %v) = %q, want %q", tt.explicit, tt.budget, got, tt.want)
			}
		})
	}
}

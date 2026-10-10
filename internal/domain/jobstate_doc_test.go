package domain

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestStateMachineDocMatchesCode keeps docs/architecture/state-machine.md
// honest: its transition table must list exactly the transitions in code.
func TestStateMachineDocMatchesCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/architecture/state-machine.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## Transition table")
	if start < 0 {
		t.Fatal("transition table section missing")
	}
	section := doc[start:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	row := regexp.MustCompile("(?m)^\\| `([A-Z_]+)` \\| (.*) \\|$")
	state := regexp.MustCompile("`([A-Z_]+)`")
	seen := map[JobState]bool{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		from := JobState(m[1])
		var got []JobState
		for _, s := range state.FindAllStringSubmatch(m[2], -1) {
			got = append(got, JobState(s[1]))
		}
		want := slices.Clone(transitions[from])
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: doc lists %v, code allows %v", from, got, want)
		}
		seen[from] = true
	}
	for from, next := range transitions {
		if len(next) > 0 && !seen[from] {
			t.Errorf("%s has transitions in code but no row in the doc", from)
		}
	}
}

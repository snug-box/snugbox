package helper

import (
	"slices"
	"strings"
	"testing"

	"snugbox.dev/snugbox/agent/internal/units"
)

// Blueprint section 5.4: the helper's ReadWritePaths is generated from the
// fix catalogue and compared in CI, so a new fix can't widen it without a
// reviewed change to WritePaths.
func TestUnitWritePaths(t *testing.T) {
	unit, err := units.Helper("/usr/libexec/snugbox/snugbox-helper")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range strings.Lines(string(unit)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ReadWritePaths="); ok {
			for _, p := range strings.Fields(v) {
				got = append(got, strings.TrimPrefix(p, "-"))
			}
		}
	}
	want := slices.Clone(WritePaths)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the unit lets the helper write %q; the fix catalogue writes %q", got, want)
	}
	if !strings.Contains(string(unit), "StateDirectory=snugbox-helper\n") || !strings.HasSuffix(StateDir, "/snugbox-helper") {
		t.Error("the helper's state folder must be its systemd StateDirectory")
	}
}

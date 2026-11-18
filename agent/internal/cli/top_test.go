package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// topOn returns a top model for a fake box with a large service that has a
// limit, a small one that waits for memory, and three idle ones.
func topOn(t *testing.T) (*topModel, *testbox.Box, *testbox.Workload) {
	t.Helper()
	b := testbox.New()
	b.Add(&testbox.Workload{Path: "system.slice/big.service", Programs: []string{"java"}, Anon: 900 * mib, Max: 2048 * mib})
	small := b.Add(&testbox.Workload{Path: "system.slice/small.service", Programs: []string{"nginx"}, Anon: 50 * mib, ActiveFile: 100 * mib})
	for i := range 3 {
		b.Add(&testbox.Workload{Path: fmt.Sprintf("system.slice/idle-%d.service", i), Programs: []string{"sleep"}, Anon: uint64(10+i) * mib})
	}
	c, err := collect.New(collect.Options{FS: b, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	return &topModel{ctx: ctx, c: c, d: model.NewDeriver(c.PageSize()), host: "web-1"}, b, small
}

func names(m *topModel) []string {
	var out []string
	for _, r := range m.rows {
		out = append(out, r.name)
	}
	return out
}

func TestTopSortsAndScrolls(t *testing.T) {
	m, b, small := topOn(t)
	if v := m.View(); !strings.Contains(v.Content, "Reading the box...") || !v.AltScreen {
		t.Errorf("before the first sample: %q", v.Content)
	}
	start := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	if _, cmd := m.Update(tickMsg(start)); cmd == nil {
		t.Fatal("a tick must schedule the next one")
	}
	b.Update(func(*testbox.Box) { small.PressureSome += 100 * time.Millisecond })
	m.Update(tickMsg(start.Add(2 * time.Second)))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 9})

	want := [][]string{
		{"big.service", "small.service", "idle-2.service", "idle-1.service", "idle-0.service"},
		{"big.service", "small.service", "idle-2.service", "idle-1.service", "idle-0.service"},
		{"small.service", "big.service", "idle-0.service", "idle-1.service", "idle-2.service"},
		{"big.service", "idle-0.service", "idle-1.service", "idle-2.service", "small.service"},
	}
	// s goes through the four orders and back to the first.
	for i, w := range want {
		if got := names(m); strings.Join(got, " ") != strings.Join(w, " ") {
			t.Errorf("sort %d (%s): %v, want %v", i, sortNames[m.sort], got, w)
		}
		m.Update(key("s"))
	}
	if m.sort != byWorkingSet {
		t.Errorf("after four presses of s the sort is %s", sortNames[m.sort])
	}

	v := m.View().Content
	for _, s := range []string{"snugbox top on web-1", "s sort by working set", "WORKLOAD", "big.service", "2,048 MiB", "5%", "2 more below"} {
		if !strings.Contains(v, s) {
			t.Errorf("the view lacks %q:\n%s", s, v)
		}
	}
	if strings.Contains(v, "idle-0.service") {
		t.Errorf("a 9-line terminal shows only 3 rows:\n%s", v)
	}
	m.Update(key("pgdown"))
	m.Update(key("down"))
	if m.offset != 4 {
		t.Errorf("offset %d after a page and a line down, want the last row, 4", m.offset)
	}
	m.Update(key("down"))
	if m.offset != 4 {
		t.Errorf("offset %d: scrolling must stop at the last row", m.offset)
	}
	m.Update(key("pgup"))
	m.Update(key("up"))
	m.Update(key("k"))
	if m.offset != 0 {
		t.Errorf("offset %d after scrolling back past the top", m.offset)
	}
}

func TestTopColoursOnlyWhenAsked(t *testing.T) {
	m, b, small := topOn(t)
	start := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	m.Update(tickMsg(start))
	b.Update(func(*testbox.Box) { small.PressureSome += 100 * time.Millisecond })
	m.Update(tickMsg(start.Add(2 * time.Second)))
	if strings.Contains(m.View().Content, "\x1b[") {
		t.Error("colour codes with colour off")
	}
	m.color, m.snug, m.conf = true, 3*model.GiB, "low"
	v := m.View().Content
	if !strings.Contains(v, "\x1b[33m") || !strings.Contains(v, "snug size 3 GiB (low confidence)") {
		t.Errorf("with colour on, pressure over 1%% is yellow and the snug size shows:\n%q", v)
	}
}

func TestTopQuits(t *testing.T) {
	m, _, _ := topOn(t)
	for _, k := range []string{"q", "esc"} {
		msg := key(k)
		if k == "esc" {
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		}
		_, cmd := m.Update(msg)
		if cmd == nil {
			t.Fatalf("%s returned no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s does not quit", k)
		}
	}
}

func TestClipName(t *testing.T) {
	if got := clipName("a-very-long-workload-name-that-goes-on", 12); got != "a-very-lo..." {
		t.Errorf("clipName = %q", got)
	}
	if got := clipName("short", 12); got != "short" {
		t.Errorf("clipName = %q", got)
	}
}

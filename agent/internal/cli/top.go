package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/report"
)

const topEvery = 2 * time.Second

func newTop(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Live view of workloads, memory and pressure",
		Long: "Show every workload's working set, the memory it can't give back, its hot file cache, its\n" +
			"limit and how much of the time it waits for memory, refreshed every 2 seconds.\n" +
			"Keys: s changes the sort, up and down scroll, q quits.",
		Example: "  sudo snugbox top",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := collect.NewLiveRoot()
			defer root.Close()
			c, err := collect.New(collect.Options{FS: root})
			if err != nil {
				return err
			}
			if err := c.Discover(cmd.Context()); err != nil {
				return err
			}
			m := &topModel{ctx: cmd.Context(), c: c, d: model.NewDeriver(c.PageSize()), host: c.Machine(cmd.Context(), nil).Hostname, color: e.color}
			if r, err := e.watcher.Result(cmd.Context()); err == nil {
				m.snug, m.conf = r.Estimate.Snug, r.Estimate.Confidence.String()
			}
			_, err = tea.NewProgram(m, tea.WithContext(cmd.Context()), tea.WithOutput(e.stdout)).Run()
			return err
		},
	}
}

type tickMsg time.Time

type topRow struct {
	id, name, kind string
	ws, hard, hot  uint64
	limit          uint64
	pressure       float64
}

type sortKey int

const (
	byWorkingSet sortKey = iota
	byHard
	byPressure
	byName
)

var sortNames = [...]string{"working set", "memory that can't be dropped", "pressure", "name"}

type topModel struct {
	ctx        context.Context
	c          *collect.Collector
	d          *model.Deriver
	host       string
	color      bool
	snug       uint64
	conf       string
	prev       model.Sample
	box        model.BoxUsage
	point      model.Point
	rows       []topRow
	sort       sortKey
	offset     int
	height     int
	width      int
	discovered time.Time
	pressure   float64
	err        error
}

func (m *topModel) Init() tea.Cmd { return m.sample }

func (m *topModel) sample() tea.Msg { return tickMsg(time.Now()) }

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "s":
			m.sort = (m.sort + 1) % sortKey(len(sortNames))
			m.order()
		case "up", "k":
			m.offset = max(m.offset-1, 0)
		case "down", "j":
			m.offset = min(m.offset+1, max(len(m.rows)-1, 0))
		case "pgdown":
			m.offset = min(m.offset+m.visible(), max(len(m.rows)-1, 0))
		case "pgup":
			m.offset = max(m.offset-m.visible(), 0)
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.refresh(time.Time(msg))
		return m, tea.Tick(topEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return m, nil
}

func (m *topModel) refresh(now time.Time) {
	if now.Sub(m.discovered) >= time.Minute {
		if err := m.c.Discover(m.ctx); err != nil {
			m.err = err
			return
		}
		m.discovered = now
	}
	s := m.c.Sample(now)
	p := m.d.Add(s)
	ws := m.c.Workloads()
	elapsed := s.Time.Sub(m.prev.Time)
	m.rows = m.rows[:0]
	for id, wp := range p.Workloads {
		w := ws[id]
		r := topRow{id: id, name: w.Label(), kind: string(w.Kind), ws: wp.WorkingSet, hard: wp.Hard, hot: wp.HotFile}
		if w.Max.Set {
			r.limit = w.Max.Bytes
		}
		if prev, ok := m.prev.Workloads[id]; ok && elapsed > 0 && s.Workloads[id].PressureSome >= prev.PressureSome {
			r.pressure = float64(s.Workloads[id].PressureSome-prev.PressureSome) / float64(elapsed)
		}
		m.rows = append(m.rows, r)
	}
	if elapsed > 0 && s.Box.PressureSome >= m.prev.Box.PressureSome {
		m.pressure = float64(s.Box.PressureSome-m.prev.Box.PressureSome) / float64(elapsed)
	}
	m.prev, m.box, m.point = s, s.Box, p
	m.order()
}

func (m *topModel) order() {
	slices.SortFunc(m.rows, func(a, b topRow) int {
		switch m.sort {
		case byHard:
			return cmpDesc(a.hard, b.hard, a.name, b.name)
		case byPressure:
			switch {
			case a.pressure > b.pressure:
				return -1
			case a.pressure < b.pressure:
				return 1
			}
			return strings.Compare(a.name, b.name)
		case byName:
			return strings.Compare(a.name, b.name)
		}
		return cmpDesc(a.ws, b.ws, a.name, b.name)
	})
}

func cmpDesc(a, b uint64, an, bn string) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return strings.Compare(an, bn)
}

func (m *topModel) visible() int { return max(m.height-6, 3) }

func (m *topModel) paint(code, s string) string {
	if !m.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (m *topModel) View() tea.View {
	var b strings.Builder
	host := m.host
	if host == "" {
		host = "this box"
	}
	fmt.Fprintf(&b, "%s  %s  %s\n", m.paint("1", "snugbox top on "+host), time.Now().Format("15:04:05"),
		m.paint("2", "s sort by "+sortNames[m.sort]+", up and down scroll, q quits"))
	if m.err != nil {
		fmt.Fprintf(&b, "%s\n", m.paint("31", "Couldn't read the cgroup tree: "+m.err.Error()))
	}
	if m.prev.Time.IsZero() {
		b.WriteString("Reading the box...\n")
		v := tea.NewView(b.String())
		v.AltScreen = true
		return v
	}
	used := uint64(0)
	if m.box.MemTotal > m.box.MemAvailable {
		used = m.box.MemTotal - m.box.MemAvailable
	}
	fmt.Fprintf(&b, "Box: %s in use of %s, working set %s, can't drop %s, pressure %s",
		report.Size(used), report.Size(m.box.MemTotal), report.Size(m.point.Total), report.Size(m.point.HardTotal), report.Percent(m.pressure))
	if m.snug > 0 {
		fmt.Fprintf(&b, ", snug size %s (%s confidence)", report.Size(m.snug), m.conf)
	}
	b.WriteString("\n\n")
	head := fmt.Sprintf("%-32s %-9s %12s %12s %12s %12s %9s", "WORKLOAD", "KIND", "WORKING SET", "CAN'T DROP", "HOT FILE", "LIMIT", "PRESSURE")
	b.WriteString(m.paint("2", head) + "\n")
	end := min(m.offset+m.visible(), len(m.rows))
	for _, r := range m.rows[min(m.offset, end):end] {
		limit := "none"
		if r.limit > 0 {
			limit = report.MiB(r.limit)
		}
		pressure := report.Percent(r.pressure)
		if r.pressure > 0.01 {
			pressure = m.paint("33", fmt.Sprintf("%9s", pressure))
		} else {
			pressure = fmt.Sprintf("%9s", pressure)
		}
		fmt.Fprintf(&b, "%-32s %-9s %12s %12s %12s %12s %s\n", clipName(r.name, 32), r.kind, report.MiB(r.ws), report.MiB(r.hard), report.MiB(r.hot), limit, pressure)
	}
	if len(m.rows) > end {
		fmt.Fprintf(&b, "%s\n", m.paint("2", fmt.Sprintf("%d more below", len(m.rows)-end)))
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func clipName(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-3]) + "..."
}

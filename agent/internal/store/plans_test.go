package store

import (
	"context"
	"strings"
	"testing"
)

// The reads the watcher makes every minute and every 5 minutes, and the
// trend window it loads at start, take their rows in an order a table's key
// or an index gives. A sort or a temporary table takes a megabyte or more on
// a full store, and the C allocator under SQLite keeps that memory once it
// is freed, which put snugboxd over its 24 MiB budget.
func TestWindowReadsNeedNoSort(t *testing.T) {
	s := open(t)
	for name, q := range map[string]string{
		"minute sums":     minuteSumsSQL,
		"minute times":    minuteTimesSQL,
		"workload hours":  workloadHoursSQL,
		"box hours":       boxHoursSQL,
		"workload points": workloadPointsSQL,
		"recent":          recentSQL,
		"minute maxima":   minuteMaximaSQL,
	} {
		if p := queryPlan(t, s, q); strings.Contains(p, "TEMP B-TREE") || strings.Contains(p, "MATERIALIZE") {
			t.Errorf("%s needs a sort or a temporary table: %s", name, p)
		}
	}
	// The check sees a sort where there is one, as in the GROUP BY the
	// workload hours read had.
	if p := queryPlan(t, s, "SELECT wid, SUM(samples) FROM wl_1h WHERE t >= ? AND t <= ? GROUP BY wid"); !strings.Contains(p, "TEMP B-TREE") {
		t.Errorf("no sort seen for a GROUP BY across the time index: %s", p)
	}
}

// queryPlan returns SQLite's plan for q, its steps joined by "; ".
func queryPlan(t *testing.T, s *Store, q string) string {
	t.Helper()
	args := make([]any, strings.Count(q, "?"))
	for i := range args {
		args[i] = 0
	}
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return strings.Join(plan, "; ")
}

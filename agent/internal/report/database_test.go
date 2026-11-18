package report

import (
	"strings"
	"testing"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Each database's note says what it holds and how far its settings may let
// it grow: what it holds plus the room the plan has beyond the snug size.
func TestDatabaseNotes(t *testing.T) {
	r := &audit.Result{
		Machine:  collect.Machine{MemTotal: 4 << 30},
		Estimate: model.Estimate{Snug: 3 << 30},
		Workloads: map[string]model.Workload{
			"/a": {ID: "/a", Name: "pg", Database: "PostgreSQL", DBSettings: []string{"shared_buffers=256MB"}},
			"/b": {ID: "/b", Name: "mysql", Database: "MySQL"},
			"/c": {ID: "/c", Name: "cache", Database: "Valkey"},
			"/d": {ID: "/d", Name: "mongo", Database: "MongoDB"},
			"/e": {ID: "/e", Name: "app"},
		},
		Summary: model.Summary{Workloads: map[string]model.WorkloadSummary{
			"/a": {WorkingSetQ999: 600 << 20, ShmemMax: 260 << 20},
			"/b": {WorkingSetQ999: 900 << 20},
			"/c": {WorkingSetQ999: 100 << 20},
			"/d": {WorkingSetQ999: 300 << 20},
		}},
	}
	notes := DatabaseNotes(r)
	if len(notes) != 4 {
		t.Fatalf("%d notes: %q", len(notes), notes)
	}
	// No plan is advised, so the notes reason about this box: 4 GiB, 1 GiB
	// more than the snug size.
	for i, want := range []string{
		"pg is PostgreSQL and holds 600 MiB, 260 MiB of it shared memory, which is mostly shared_buffers. On this box, shared_buffers plus max_connections times work_mem should stay under 1,624 MiB",
		"mysql is MySQL and holds 900 MiB, most of it usually the InnoDB buffer pool. On this box, innodb_buffer_pool_size should stay under 1,924 MiB.",
		"cache is Valkey and holds 100 MiB. On this box, set maxmemory under 1,124 MiB: without it, the default, Valkey grows until the box runs out of memory.",
		"mongo is MongoDB and holds 300 MiB. Its WiredTiger cache defaults to half of RAM less 1 GiB and may not see a container's limit, so on this box, set --wiredTigerCacheSizeGB to keep the whole of it under 1,324 MiB.",
	} {
		if !strings.HasPrefix(notes[i], want) {
			t.Errorf("note %d\n got %q\nwant it to start %q", i, notes[i], want)
		}
	}
	if !strings.HasSuffix(notes[0], " Its command line sets shared_buffers=256MB.") {
		t.Errorf("the PostgreSQL note doesn't name its command-line setting: %q", notes[0])
	}
}

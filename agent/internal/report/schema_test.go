package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/fix"
)

// schemaFile is the JSON Schema of the JSON report, which the site publishes
// at the URL in SchemaV1.
var schemaFile = filepath.Join("..", "..", "..", "schema", "audit", "v1.json")

// fullResult is the shop box with every optional part of the JSON report
// filled in: a slice policy, the instance type, every kind of risk detail,
// a swap fix, a schedule fix with a timer and a cron job, PHP-FPM advice, an
// idle container, a Compose project, database settings and a plan that
// can't be ordered.
func fullResult(t *testing.T) *audit.Result {
	t.Helper()
	r := shopResult(t)
	r.Machine.OOMPolicy = collect.OOMPolicy{Slice: "system.slice", Value: "kill", File: "/usr/lib/systemd/system/system.slice.d/10-oomd-per-slice-defaults.conf"}
	r.Provider.InstanceType = "s-4vcpu-8gb"
	r.Risks = append(r.Risks,
		audit.Risk{Kind: audit.RiskKills, Count: 2, Undated: 1, Names: []string{"shop-app-1"}},
		audit.Risk{Kind: audit.RiskPressure, Share: 0.04},
		audit.Risk{Kind: audit.RiskTHPDatabase, Setting: "always", Database: "Redis"},
		audit.Risk{Kind: audit.RiskCrashLoop, Count: 1, Names: []string{"shop-worker-1"}, Restarts: []int{14}},
		audit.Risk{Kind: audit.RiskOrphanBrowsers, Count: 1, Names: []string{"render.service"}, Bytes: 300 * mib},
	)
	r.Fixes = append(r.Fixes,
		fix.Proposal{Kind: fix.KindSwap, Risk: fix.RiskLow, SwapSize: 2048 * mib, Zswap: true, Compressor: "zstd"},
		fix.Proposal{Kind: fix.KindHeap, Risk: fix.RiskHigh, Heaps: []fix.Heap{{WorkloadID: "/system.slice/php8.3-fpm.service", Name: "php8.3-fpm.service",
			Runtime: "php-fpm", Setting: "pm.max_children = 12", Limit: 1024 * mib, Workers: 20, WorkerBytes: 70 * mib}}},
		fix.Proposal{Kind: fix.KindSchedule, Risk: fix.RiskLow,
			Shifts: []fix.Shift{{Timer: "dump.timer", Service: "dump.service", Minutes: 20, With: "backup.service", Days: 5, Peak: 900 * mib}},
			Cron: []fix.CronMove{{File: "/etc/cron.d/reports", Line: 3, Program: "/usr/local/bin/reports", Schedule: "0 2 * * *", Minutes: 30,
				Suggested: "30 2 * * *", With: "backup.service", At: time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC), Days: 4, Rise: 600 * mib}}},
	)
	r.Idle = append(r.Idle, audit.Idle{ID: "/system.slice/docker-old.scope", Name: "shop-old-1", CPU: 3 * time.Second, Observed: 8 * 24 * time.Hour, WorkingSet: 120 * mib})
	for id, w := range r.Workloads {
		w.Compose.Project, w.Compose.Service = "shop", "db"
		w.Database, w.DBSettings = "PostgreSQL", []string{"shared_buffers=128MB"}
		r.Workloads[id] = w
		break
	}
	if len(r.Advice.Options) == 0 {
		t.Fatal("the shop box has no plan to list as one that can't be ordered")
	}
	r.Advice.Unavailable = append(r.Advice.Unavailable, r.Advice.Options[0])
	return r
}

// The JSON report keeps to schema/audit/v1.json, for the shop box and for
// one with every optional part. Readers must ignore fields they don't know,
// so the schema allows more than it names; this test doesn't, and fails on
// any field the schema doesn't describe, so the schema can't fall behind.
func TestJSONMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaV1, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	if tree["$id"] != SchemaV1 {
		t.Errorf("the schema's $id is %v, want %s", tree["$id"], SchemaV1)
	}
	for name, r := range map[string]*audit.Result{"shop": shopResult(t), "full": fullResult(t)} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := JSON(&buf, r, "1.0.0"); err != nil {
				t.Fatal(err)
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("the report doesn't match %s: %v", schemaFile, err)
			}
			for _, p := range undeclared(tree, tree, inst, "") {
				t.Errorf("%s is in the report but not in %s", p, schemaFile)
			}
		})
	}
	var full bytes.Buffer
	if err := JSON(&full, fullResult(t), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"oom_policy": {`, `"instance_type"`, `"restarts"`, `"swap_bytes"`, `"workers"`, `"shifts"`, `"suggested_schedule"`, `"compose"`, `"database_settings"`, `"idle": [`} {
		if !strings.Contains(full.String(), key) {
			t.Errorf("the full report has no %s, so the schema test doesn't reach it", key)
		}
	}
}

// undeclared finds a key the schema doesn't name at any depth, through $ref,
// anyOf and array items, so TestJSONMatchesSchema can't pass by missing one.
func TestUndeclaredFindsNewFields(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(`{
		"properties": {"a": {"$ref": "#/$defs/thing"}, "list": {"type": "array", "items": {"$ref": "#/$defs/thing"}}},
		"$defs": {"thing": {"anyOf": [{"type": "object", "properties": {"b": {"type": "integer"}}}, {"type": "null"}]}}
	}`), &root); err != nil {
		t.Fatal(err)
	}
	var inst any
	if err := json.Unmarshal([]byte(`{"a": {"b": 1, "new": 2}, "list": [{"b": 1}, {"b": 2, "other": 3}], "top": true}`), &inst); err != nil {
		t.Fatal(err)
	}
	got := undeclared(root, root, inst, "")
	if want := []string{".a.new", ".list[1].other", ".top"}; !slices.Equal(got, want) {
		t.Errorf("undeclared = %q, want %q", got, want)
	}
}

// undeclared lists the object keys in v that the schema node s doesn't name
// in its properties, as JSON paths. root is the whole schema, for $ref.
func undeclared(root, s map[string]any, v any, at string) []string {
	s = resolve(root, s)
	switch v := v.(type) {
	case map[string]any:
		props, ok := s["properties"].(map[string]any)
		if !ok {
			if branch := objectBranch(root, s); branch != nil {
				return undeclared(root, branch, v, at)
			}
			return []string{at + " (an object the schema has no properties for)"}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var out []string
		for _, k := range keys {
			sub, ok := props[k].(map[string]any)
			if !ok {
				out = append(out, at+"."+k)
				continue
			}
			out = append(out, undeclared(root, sub, v[k], at+"."+k)...)
		}
		return out
	case []any:
		items, _ := s["items"].(map[string]any)
		var out []string
		for i, e := range v {
			out = append(out, undeclared(root, items, e, fmt.Sprintf("%s[%d]", at, i))...)
		}
		return out
	}
	return nil
}

// resolve follows $ref to the schema's $defs.
func resolve(root, s map[string]any) map[string]any {
	for s != nil {
		ref, ok := s["$ref"].(string)
		if !ok {
			break
		}
		defs, _ := root["$defs"].(map[string]any)
		s, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
	return s
}

// objectBranch returns the branch of an anyOf that describes an object.
func objectBranch(root, s map[string]any) map[string]any {
	branches, _ := s["anyOf"].([]any)
	for _, b := range branches {
		if m, ok := b.(map[string]any); ok {
			if m = resolve(root, m); m != nil && m["properties"] != nil {
				return m
			}
		}
	}
	return nil
}

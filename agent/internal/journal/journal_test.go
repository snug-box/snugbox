package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func clock() func() time.Time {
	t := time.Date(2026, 10, 31, 10, 30, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

// openFile opens a journal the way the helper does on Linux, appending.
// Windows can't cut a file opened for appending, so there it is opened for
// writing at the end, which Open seeks to.
func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	flags := os.O_RDWR | os.O_APPEND | os.O_CREATE
	if runtime.GOOS == "windows" {
		flags &^= os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// sample writes a journal of one transaction and returns its path.
func sample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal")
	j, torn, err := Open(openFile(t, path), clock())
	if err != nil || torn != 0 {
		t.Fatalf("Open: %v, torn %d", err, torn)
	}
	steps := []struct {
		state string
		data  any
	}{
		{"approved", map[string]any{"fix": "service-limits", "unit": "app.service", "max": 805306368}},
		{"preflight", nil},
		{"snapshot", map[string]string{"manifest": "sha256 of the manifest"}},
		{"applying", nil},
		{"verifying", nil},
	}
	for _, s := range steps {
		if _, err := j.Append("20261031-103000-service-limits", s.state, "root", "", s.data); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.SplitAfter(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

func write(t *testing.T, path string, ls [][]byte) {
	t.Helper()
	b := bytes.Join(ls, nil)
	if !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) ([]Entry, int, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return Read(f)
}

func TestAppendAndReadBack(t *testing.T) {
	path := sample(t)
	entries, torn, err := readFile(t, path)
	if err != nil || torn != 0 {
		t.Fatalf("Read: %v, torn %d", err, torn)
	}
	if len(entries) != 5 {
		t.Fatalf("read %d entries, want 5", len(entries))
	}
	prev := Genesis
	for i, e := range entries {
		if e.Seq != uint64(i+1) || e.Prev != prev || len(e.Hash) != 64 {
			t.Errorf("entry %d: seq %d prev %.8s hash %q", i, e.Seq, e.Prev, e.Hash)
		}
		prev = e.Hash
	}
	if entries[0].State != "approved" || !strings.Contains(string(entries[0].Data), `"unit":"app.service"`) {
		t.Errorf("first entry = %+v", entries[0])
	}

	j, _, err := Open(openFile(t, path), clock())
	if err != nil {
		t.Fatal(err)
	}
	e, err := j.Append("20261031-103000-service-limits", "watching", "snugboxd", "30 quiet minutes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 6 || e.Prev != entries[4].Hash {
		t.Errorf("appended after reopening: seq %d prev %.8s, want 6 and %.8s", e.Seq, e.Prev, entries[4].Hash)
	}
	if again, _, err := readFile(t, path); err != nil || len(again) != 6 {
		t.Errorf("after reopening and appending: %d entries, %v", len(again), err)
	}
}

// Blueprint section 16.2: the hash chain is verified by a test that edits a
// line and expects a failure.
func TestEditsAreFound(t *testing.T) {
	cases := []struct {
		name     string
		edit     func(ls [][]byte) [][]byte
		wantLine int
	}{
		{"one byte of a state", func(ls [][]byte) [][]byte {
			ls[1] = bytes.Replace(ls[1], []byte(`"preflight"`), []byte(`"preflighT"`), 1)
			return ls
		}, 2},
		{"a value in the data", func(ls [][]byte) [][]byte {
			ls[0] = bytes.Replace(ls[0], []byte(`805306368`), []byte(`905306368`), 1)
			return ls
		}, 1},
		{"a space that parses the same", func(ls [][]byte) [][]byte {
			ls[2] = bytes.Replace(ls[2], []byte(`"state":`), []byte(`"state": `), 1)
			return ls
		}, 3},
		{"a line taken out", func(ls [][]byte) [][]byte {
			return append(ls[:2:2], ls[3:]...)
		}, 3},
		{"two lines swapped", func(ls [][]byte) [][]byte {
			ls[1], ls[2] = ls[2], ls[1]
			return ls
		}, 2},
		{"a line rewritten with a fresh hash", func(ls [][]byte) [][]byte {
			e, err := decode(bytes.TrimSuffix(ls[3], []byte("\n")))
			if err != nil {
				t.Fatal(err)
			}
			e.Note = "nothing happened here"
			line, _, err := encode(e)
			if err != nil {
				t.Fatal(err)
			}
			ls[3] = line
			return ls
		}, 5},
		{"an extra field", func(ls [][]byte) [][]byte {
			e, err := decode(bytes.TrimSuffix(ls[0], []byte("\n")))
			if err != nil {
				t.Fatal(err)
			}
			line, _, err := encode(e)
			if err != nil {
				t.Fatal(err)
			}
			ls[0] = bytes.Replace(line, []byte(`{"seq"`), []byte(`{"extra":1,"seq"`), 1)
			return ls
		}, 1},
		{"the hash taken off", func(ls [][]byte) [][]byte {
			i := bytes.LastIndex(ls[4], hashOpen)
			ls[4] = append(append([]byte(nil), ls[4][:i]...), "}\n"...)
			return ls
		}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := sample(t)
			write(t, path, tc.edit(lines(t, path)))
			_, _, err := readFile(t, path)
			var be *BrokenError
			if !errors.As(err, &be) {
				t.Fatalf("Read = %v, want a BrokenError", err)
			}
			if be.Line != tc.wantLine {
				t.Errorf("broken at line %d (%s), want line %d", be.Line, be.Reason, tc.wantLine)
			}
			if _, _, err := Open(openFile(t, path), clock()); err == nil {
				t.Error("Open accepted the edited journal")
			}
		})
	}
}

func TestTornLastLineIsCut(t *testing.T) {
	path := sample(t)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	half := []byte(`{"seq":6,"time":"2026-10-31T10:31:00Z","tx":"20261031-1030`)
	if err := os.WriteFile(path, append(bytes.Clone(whole), half...), 0o600); err != nil {
		t.Fatal(err)
	}
	j, torn, err := Open(openFile(t, path), clock())
	if err != nil {
		t.Fatal(err)
	}
	if torn != len(half) {
		t.Errorf("torn = %d, want %d", torn, len(half))
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("the torn line was not cut: %v", err)
	}
	if _, err := j.Append("20261031-103000-service-limits", "undone", "helper", "the helper restarted", nil); err != nil {
		t.Fatal(err)
	}
	if entries, torn, err := readFile(t, path); err != nil || torn != 0 || len(entries) != 6 {
		t.Errorf("after the cut: %d entries, torn %d, %v", len(entries), torn, err)
	}
}

func TestEmptyAndOversized(t *testing.T) {
	entries, torn, err := Read(strings.NewReader(""))
	if err != nil || torn != 0 || len(entries) != 0 {
		t.Errorf("empty journal: %d entries, torn %d, %v", len(entries), torn, err)
	}
	long := strings.Repeat("x", maxLine+10) + "\n"
	if _, _, err := Read(strings.NewReader(long)); err == nil {
		t.Error("a line longer than the limit was accepted")
	}
}

func TestClockSteppingBackKeepsOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	at := time.Date(2026, 10, 31, 9, 45, 31, 0, time.UTC)
	j, _, err := Open(openFile(t, path), func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	first, err := j.Append("tx", "applying", "root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	at = at.Add(-400 * time.Millisecond)
	second, err := j.Append("tx", "undone", "helper", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Time.Before(first.Time) {
		t.Errorf("the second line is stamped %v, before the first at %v", second.Time, first.Time)
	}
}

func TestAppendRefusesAHugeStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	j, _, err := Open(openFile(t, path), clock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append("tx", "approved", "root", "", strings.Repeat("y", maxLine)); err == nil {
		t.Error("a step longer than a line may be was written")
	}
	if len(j.Entries()) != 0 {
		t.Error("a refused step was kept")
	}
}

func FuzzRead(f *testing.F) {
	path := filepath.Join(f.TempDir(), "journal")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		f.Fatal(err)
	}
	j, _, err := Open(file, clock())
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"approved", "preflight", "undone"} {
		if _, err := j.Append("20261031-103000-swap", s, "root", "note", map[string]int{"n": 1}); err != nil {
			f.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte(`{"seq":1,"hash":"` + Genesis + `"}` + "\n"))
	f.Add([]byte("\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, torn, err := Read(bytes.NewReader(data))
		if err != nil {
			return
		}
		if torn < 0 || torn > len(data) {
			t.Fatalf("torn = %d for %d bytes", torn, len(data))
		}
		prev := Genesis
		for i, e := range entries {
			if e.Seq != uint64(i+1) || e.Prev != prev {
				t.Fatalf("accepted a broken chain at entry %d", i)
			}
			prev = e.Hash
		}
	})
}

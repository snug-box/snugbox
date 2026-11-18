// Package journal is the helper's record of every change it makes, rule 5 of
// blueprint section 3.7: one JSON line per step, each holding the SHA-256 of
// the line before it, so no line can be edited, removed or moved afterwards
// without Verify finding it.
//
// A line's hash covers its exact bytes up to the hash field, so even a change
// that parses the same is found.
package journal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// MaxSize is the largest journal Open reads. A transaction writes about ten
// lines of a few hundred bytes, so this is many lifetimes of fixes.
const MaxSize = 64 << 20

// maxLine bounds one line; the largest data a step records is a few file
// contents of a few KiB.
const maxLine = 1 << 20

// Genesis is the prev value of the first line.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Entry is one line of the journal.
type Entry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Tx    string    `json:"tx"`
	State string    `json:"state"`
	// By is who took the step: root or snugboxd for the steps they asked
	// for, and helper for those the helper takes itself, such as the
	// preflight checks or finishing a transaction after a restart.
	By   string          `json:"by"`
	Note string          `json:"note,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Prev string          `json:"prev"`
	Hash string          `json:"-"`
}

// BrokenError says where and how a journal fails verification.
type BrokenError struct {
	Line   int
	Reason string
}

func (e *BrokenError) Error() string {
	return fmt.Sprintf("journal line %d: %s", e.Line, e.Reason)
}

// suffix is how every line ends: the hash field, then the closing brace.
var (
	hashOpen  = []byte(`,"hash":"`)
	hashClose = []byte(`"}`)
)

// encode returns the line for e, newline included, and its hash.
func encode(e Entry) ([]byte, string, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, "", fmt.Errorf("journal: %w", err)
	}
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	line := make([]byte, 0, len(body)+len(hashOpen)+len(h)+len(hashClose)+1)
	line = append(line, body[:len(body)-1]...)
	line = append(line, hashOpen...)
	line = append(line, h...)
	line = append(line, hashClose...)
	return append(line, '\n'), h, nil
}

// decode checks one line, without its newline, and returns its entry.
func decode(line []byte) (Entry, error) {
	tail := len(hashOpen) + 2*sha256.Size + len(hashClose)
	if len(line) < tail+2 || !bytes.HasSuffix(line, hashClose) {
		return Entry{}, errors.New("no hash at the end of the line")
	}
	at := len(line) - tail
	if !bytes.Equal(line[at:at+len(hashOpen)], hashOpen) {
		return Entry{}, errors.New("no hash at the end of the line")
	}
	h := string(line[at+len(hashOpen) : len(line)-len(hashClose)])
	body := make([]byte, 0, at+1)
	body = append(body, line[:at]...)
	body = append(body, '}')
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != h {
		return Entry{}, errors.New("the line was changed after it was written")
	}
	var e Entry
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Entry{}, fmt.Errorf("the line is not a journal entry: %w", err)
	}
	e.Hash = h
	return e, nil
}

// Read verifies a whole journal and returns its entries. A last line without
// a newline is a write the machine stopped in the middle of; it is reported
// in torn, as a byte count to cut, and not returned as an entry.
func Read(r io.Reader) (entries []Entry, torn int, err error) {
	br := bufio.NewReaderSize(io.LimitReader(r, MaxSize+1), 64<<10)
	prev := Genesis
	var size int
	for n := 1; ; n++ {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			line, err = readLong(br, line)
		}
		size += len(line)
		if size > MaxSize {
			return nil, 0, fmt.Errorf("journal: larger than %d bytes", MaxSize)
		}
		switch {
		case errors.Is(err, io.EOF) && len(line) == 0:
			return entries, 0, nil
		case errors.Is(err, io.EOF):
			return entries, len(line), nil
		case err != nil:
			return nil, 0, fmt.Errorf("journal: %w", err)
		}
		e, derr := decode(line[:len(line)-1])
		if derr != nil {
			return nil, 0, &BrokenError{Line: n, Reason: derr.Error()}
		}
		if e.Seq != uint64(n) {
			return nil, 0, &BrokenError{Line: n, Reason: fmt.Sprintf("numbered %d; a line before it is missing or moved", e.Seq)}
		}
		if e.Prev != prev {
			return nil, 0, &BrokenError{Line: n, Reason: "does not follow the line before it"}
		}
		prev = e.Hash
		entries = append(entries, e)
	}
}

// readLong finishes a line longer than the reader's buffer.
func readLong(br *bufio.Reader, first []byte) ([]byte, error) {
	line := append([]byte(nil), first...)
	for {
		more, err := br.ReadSlice('\n')
		line = append(line, more...)
		if len(line) > maxLine {
			return nil, fmt.Errorf("a line is longer than %d bytes", maxLine)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// Journal appends to a verified journal file.
type Journal struct {
	f       *os.File
	entries []Entry
	now     func() time.Time
}

// Open verifies the journal in f and returns it ready to append. f must be
// open for reading and appending. A torn last line, left by a crash in the
// middle of a write, is cut off; Torn reports how many bytes went.
func Open(f *os.File, now func() time.Time) (*Journal, int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("journal: %w", err)
	}
	entries, torn, err := Read(f)
	if err != nil {
		return nil, 0, err
	}
	if torn > 0 {
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, 0, fmt.Errorf("journal: %w", err)
		}
		if err := f.Truncate(end - int64(torn)); err != nil {
			return nil, 0, fmt.Errorf("journal: cut the torn line: %w", err)
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return nil, 0, fmt.Errorf("journal: %w", err)
	}
	return &Journal{f: f, entries: entries, now: now}, torn, nil
}

// Entries returns every entry, oldest first. The caller must not change
// them.
func (j *Journal) Entries() []Entry { return j.entries }

// Close closes the journal file.
func (j *Journal) Close() error { return j.f.Close() }

// Head is the newest entry, or a zero Entry for an empty journal.
func (j *Journal) Head() Entry {
	if len(j.entries) == 0 {
		return Entry{Hash: Genesis}
	}
	return j.entries[len(j.entries)-1]
}

// Append writes one step and syncs it to disk before returning, so a step is
// never acted on before it is recorded. A line's time never goes before the
// line above it: when the clock steps back, as NTP or a virtual machine's
// time sync can make it, the record stays in order.
func (j *Journal) Append(tx, state, by, note string, data any) (Entry, error) {
	now := j.now().UTC()
	if head := j.Head(); now.Before(head.Time) {
		now = head.Time
	}
	e := Entry{Seq: uint64(len(j.entries)) + 1, Time: now, Tx: tx, State: state, By: by, Note: note, Prev: j.Head().Hash}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return Entry{}, fmt.Errorf("journal: %w", err)
		}
		e.Data = raw
	}
	line, h, err := encode(e)
	if err != nil {
		return Entry{}, err
	}
	if len(line) > maxLine {
		return Entry{}, fmt.Errorf("journal: a step of %d bytes is longer than %d", len(line), maxLine)
	}
	if _, err := j.f.Write(line); err != nil {
		return Entry{}, fmt.Errorf("journal: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return Entry{}, fmt.Errorf("journal: %w", err)
	}
	e.Hash = h
	j.entries = append(j.entries, e)
	return e, nil
}

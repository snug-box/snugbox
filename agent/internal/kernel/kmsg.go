package kernel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// KmsgRecord is one record read from /dev/kmsg, as described in the kernel's
// Documentation/ABI/testing/dev-kmsg.
type KmsgRecord struct {
	Priority int
	Seq      uint64
	// SinceBoot is the record's timestamp on the monotonic clock.
	SinceBoot time.Duration
	Message   string
}

// ParseKmsgRecord parses one record: "prio,seq,usec,flags[,more];message",
// optionally followed by continuation lines that start with a space. The
// continuation lines are dropped.
func ParseKmsgRecord(b []byte) (KmsgRecord, error) {
	head, msg, ok := strings.Cut(string(b), ";")
	if !ok {
		return KmsgRecord{}, errors.New("kmsg: no ';' separator")
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	f := strings.Split(head, ",")
	if len(f) < 4 {
		return KmsgRecord{}, fmt.Errorf("kmsg: %d header fields", len(f))
	}
	prio, err := strconv.Atoi(f[0])
	if err != nil || prio < 0 {
		return KmsgRecord{}, fmt.Errorf("kmsg priority %q", f[0])
	}
	seq, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return KmsgRecord{}, fmt.Errorf("kmsg sequence %q", f[1])
	}
	us, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || us < 0 || us > int64(1<<62)/int64(time.Microsecond) {
		return KmsgRecord{}, fmt.Errorf("kmsg timestamp %q", f[2])
	}
	return KmsgRecord{Priority: prio, Seq: seq, SinceBoot: time.Duration(us) * time.Microsecond, Message: msg}, nil
}

// OOMKill is what the kernel reports when its out-of-memory killer, or a
// cgroup's, kills a task.
type OOMKill struct {
	PID int
	// Task is the command name of the killed task, at most 15 bytes.
	Task string
	// Memcg is the cgroup the task ran in, from task_memcg, empty when the
	// kernel did not say.
	Memcg string
	// Constraint is CONSTRAINT_NONE for a box-wide kill and CONSTRAINT_MEMCG
	// when a cgroup's limit was hit.
	Constraint string
}

// ParseOOMKill reads the one-line summary that Linux has printed since 4.19:
//
//	oom-kill:constraint=CONSTRAINT_MEMCG,...,task_memcg=/system.slice/x.service,task=x,pid=123,uid=0
func ParseOOMKill(msg string) (OOMKill, bool) {
	rest, ok := strings.CutPrefix(msg, "oom-kill:")
	if !ok {
		return OOMKill{}, false
	}
	var k OOMKill
	hasPID := false
	for kv := range strings.SplitSeq(rest, ",") {
		key, val, _ := strings.Cut(kv, "=")
		switch key {
		case "constraint":
			k.Constraint = val
		case "task_memcg":
			k.Memcg = val
		case "task":
			k.Task = val
		case "pid":
			pid, err := strconv.Atoi(val)
			if err != nil || pid <= 0 {
				return OOMKill{}, false
			}
			k.PID, hasPID = pid, true
		}
	}
	return k, hasPID
}

// KilledProcess is the detail line the kernel prints for every kill:
//
//	Out of memory: Killed process 123 (java) total-vm:..., anon-rss:2048kB, ...
//
// A cgroup kill starts with "Memory cgroup out of memory:" instead.
type KilledProcess struct {
	PID     int
	Task    string
	AnonRSS uint64
}

// ParseKilledProcess parses the detail line of an out-of-memory kill.
func ParseKilledProcess(msg string) (KilledProcess, bool) {
	_, rest, ok := strings.Cut(msg, "out of memory: Killed process ")
	if !ok {
		_, rest, ok = strings.Cut(msg, "Out of memory: Killed process ")
	}
	if !ok {
		return KilledProcess{}, false
	}
	pidText, rest, ok := strings.Cut(rest, " (")
	if !ok {
		return KilledProcess{}, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return KilledProcess{}, false
	}
	end := strings.LastIndex(rest, ")")
	if end < 0 {
		return KilledProcess{}, false
	}
	kp := KilledProcess{PID: pid, Task: rest[:end]}
	for field := range strings.FieldsSeq(rest[end+1:]) {
		if v, ok := strings.CutPrefix(strings.TrimSuffix(field, ","), "anon-rss:"); ok {
			if n, err := strconv.ParseUint(strings.TrimSuffix(v, "kB"), 10, 64); err == nil {
				if b, err := kibToBytes(n); err == nil {
					kp.AnonRSS = b
				}
			}
		}
	}
	return kp, true
}

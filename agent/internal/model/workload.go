package model

import "time"

// Limit is a memory limit in bytes. Set is false when there is none.
type Limit struct {
	Bytes uint64
	Set   bool
}

// Compose names the Docker Compose project and service a container belongs
// to, from the labels Compose sets.
type Compose struct {
	Project    string
	Service    string
	WorkingDir string
}

// Workload describes one container, systemd service or group of processes.
type Workload struct {
	// ID is the cgroup path below the cgroup root, such as
	// "/system.slice/nginx.service", or "proc:<program>" for processes that
	// sit in the root cgroup and are grouped by program name.
	ID   string
	Kind Kind
	// Name is the container name, the unit name or the program name.
	Name string
	// Programs lists up to three program names, the largest first.
	Programs    []string
	ContainerID string
	Compose     Compose
	// Database names the database the workload runs, such as "PostgreSQL",
	// when one of its programs is a known database server, and DBSettings
	// the memory settings its main process was started with, such as
	// "shared_buffers=256MB". No other part of its command line is kept.
	Database   string
	DBSettings []string
	// Runtime names a language runtime whose heap size Snugbox can cap, and
	// HeapFlags holds the heap flags it was started with. Only flags from a
	// fixed list are kept, because command lines can hold passwords.
	Runtime   string
	HeapFlags []string
	// Threads is the thread count of the runtime's largest process, which
	// decides whether glibc's per-thread malloc arenas are worth capping.
	Threads int
	// Workers and WorkerBytes describe PHP-FPM: how many worker processes
	// run under its master now, and the largest proportional set size one
	// of them has had since Snugbox started looking.
	Workers     int
	WorkerBytes uint64
	// OrphanBrowsers counts headless browsers whose parent has gone, with
	// OrphanProcesses their processes, OrphanBytes their proportional set
	// sizes added up, and OrphanProgram the browser's name.
	OrphanBrowsers  int
	OrphanProcesses int
	OrphanBytes     uint64
	OrphanProgram   string
	Max             Limit
	High            Limit
	// Reservation is Docker's soft limit, --memory-reservation.
	Reservation Limit
	// Restarts is Docker's restart count for the container.
	Restarts int
	// FromSmaps is true for process groups measured from smaps_rollup, which
	// has no file cache, pressure or kill counters.
	FromSmaps bool
}

// Label is the name to show for the workload: Name, or the ID when there is
// no name.
func (w Workload) Label() string {
	if w.Name != "" {
		return w.Name
	}
	return w.ID
}

// Run is one run of a service: from when its cgroup appeared to when it
// went, and the highest working set it reached.
type Run struct {
	Workload, Name string
	Start, End     time.Time
	Peak           uint64
}

// Timer is a systemd timer and the service it starts. Moved is when Snugbox
// moved it earlier, and zero when it hasn't.
type Timer struct {
	Name, Service string
	Moved         time.Time
}

// MinuteTotal is the box's highest working set in one minute of the
// watcher's history.
type MinuteTotal struct {
	Time  time.Time
	Total uint64
}

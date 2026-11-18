// Package cron reads crontabs and works out when their jobs start, for the
// schedule fix's advice on cron jobs that keep starting together with
// another heavy job (blueprint section 3.7). It reads the five-field
// schedules of Vixie cron and cronie, with names, ranges, steps, lists and
// the @ words, and it never runs or changes anything.
package cron

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Job is one line of a crontab that starts a command.
type Job struct {
	// File and Line say where the job is written.
	File string
	Line int
	// Spec is the schedule as written, such as "0 2 * * *" or "@daily".
	Spec string
	// Program is the base name of the program the command runs, such as
	// "backup.sh". The rest of the command line is never kept, since it can
	// hold a password.
	Program string

	// fields is Spec with an @ word written out; it is nil for @reboot,
	// which starts the job at boot only.
	fields []string
	when   schedule
}

type field struct {
	set uint64
	// star is true when the field starts with "*", which decides how the
	// two day fields combine.
	star bool
}

func (f field) has(n int) bool { return f.set&(1<<uint(n)) != 0 }

type schedule struct{ minute, hour, dom, month, dow field }

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// Parse reads one crontab. A system crontab, /etc/crontab or a file in
// /etc/cron.d, names the user before the command; a user's own crontab
// doesn't. Lines Parse can't read are left out. A crontab that sets
// CRON_TZ or TZ gives no jobs at all, since its times are in a zone of
// their own.
func Parse(file string, data []byte, system bool) []Job {
	var jobs []Job
	n := 0
	for line := range strings.Lines(string(data)) {
		n++
		text := strings.TrimSpace(line)
		if text == "" || text[0] == '#' {
			continue
		}
		if name, _, ok := strings.Cut(text, "="); ok && text[0] != '@' && (text[0] < '0' || text[0] > '9') && text[0] != '*' {
			if name = strings.TrimSpace(name); name == "CRON_TZ" || name == "TZ" {
				return nil
			}
			continue
		}
		if j, ok := parseLine(text, system); ok {
			j.File, j.Line = file, n
			jobs = append(jobs, j)
		}
	}
	return jobs
}

func parseLine(text string, system bool) (Job, bool) {
	words := strings.Fields(text)
	var j Job
	var rest []string
	if strings.HasPrefix(words[0], "@") {
		j.Spec, rest = words[0], words[1:]
		if j.Spec != "@reboot" {
			m, ok := macros[strings.ToLower(j.Spec)]
			if !ok {
				return Job{}, false
			}
			j.fields = strings.Fields(m)
		}
	} else {
		if len(words) < 6 {
			return Job{}, false
		}
		j.fields, rest = words[:5], words[5:]
		j.Spec = strings.Join(j.fields, " ")
	}
	if system {
		if len(rest) < 2 {
			return Job{}, false
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return Job{}, false
	}
	if j.fields != nil {
		w, err := parseSchedule(j.fields)
		if err != nil {
			return Job{}, false
		}
		j.when = w
	}
	j.Program = program(rest)
	return j, true
}

func parseSchedule(f []string) (schedule, error) {
	var s schedule
	var err error
	if s.minute, err = parseField(f[0], 0, 59, nil); err != nil {
		return s, err
	}
	if s.hour, err = parseField(f[1], 0, 23, nil); err != nil {
		return s, err
	}
	if s.dom, err = parseField(f[2], 1, 31, nil); err != nil {
		return s, err
	}
	if s.month, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return s, err
	}
	// Both 0 and 7 are Sunday.
	if s.dow, err = parseField(f[4], 0, 7, dayNames); err != nil {
		return s, err
	}
	if s.dow.has(7) {
		s.dow.set |= 1
	}
	return s, nil
}

// parseField reads one schedule field: "*", a number or name, a range, any
// of those with a step, or a list of them.
func parseField(text string, lo, hi int, names map[string]int) (field, error) {
	f := field{star: strings.HasPrefix(text, "*")}
	for part := range strings.SplitSeq(text, ",") {
		spec, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 || n > hi {
				return field{}, fmt.Errorf("cron: bad step in %q", part)
			}
			step = n
		}
		var from, to int
		switch a, b, isRange := strings.Cut(spec, "-"); {
		case spec == "*":
			from, to = lo, hi
		case isRange:
			var err error
			if from, err = number(a, lo, hi, names); err != nil {
				return field{}, err
			}
			if to, err = number(b, lo, hi, names); err != nil {
				return field{}, err
			}
			if to < from {
				return field{}, fmt.Errorf("cron: range %q runs backwards", spec)
			}
		case hasStep:
			// "5/10" is not cron; a step needs "*" or a range before it.
			return field{}, fmt.Errorf("cron: a step needs a range in %q", part)
		default:
			n, err := number(spec, lo, hi, names)
			if err != nil {
				return field{}, err
			}
			from, to = n, n
		}
		for n := from; n <= to; n += step {
			f.set |= 1 << uint(n)
		}
	}
	return f, nil
}

func number(text string, lo, hi int, names map[string]int) (int, error) {
	if n, ok := names[strings.ToLower(text)]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < lo || n > hi || strings.HasPrefix(text, "+") {
		return 0, fmt.Errorf("cron: %q is not a value from %d to %d", text, lo, hi)
	}
	return n, nil
}

// Words that start a command line without being the program it runs. A
// wrapper's options come next, then as many words as positional says,
// before the program.
var (
	keywords   = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true, "while": true, "until": true, "for": true, "!": true}
	positional = map[string]int{"cd": 1, "flock": 1, "timeout": 1, "nice": 0, "ionice": 0, "nohup": 0, "exec": 0, "env": 0, "sudo": 0, "chronic": 0, "command": 0, "sh": 0, "bash": 0}
	valueOpts  = map[string][]string{"sudo": {"-u", "-g", "-U", "-C"}, "nice": {"-n"}, "ionice": {"-c", "-n", "-p"}, "flock": {"-w", "-E"}, "timeout": {"-s", "-k"}}
)

// program guesses, for people, the program a command line runs: the first
// word of the first part between &&, ||, ; and | that isn't a test, a shell
// word, a wrapper such as nice or flock with its arguments, an option, a
// number or a variable. The line is cut at the first % that isn't escaped,
// which starts the command's standard input. The file and line say for
// certain which job it is.
func program(words []string) string {
	cmd := strings.Join(words, " ")
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == '\\' {
			i++
			continue
		}
		if cmd[i] == '%' {
			cmd = cmd[:i]
			break
		}
	}
	var part []string
	next := func() string {
		p := partProgram(part)
		part = part[:0]
		return p
	}
	for w := range strings.FieldsSeq(cmd) {
		if w == "&&" || w == "||" || w == "|" || w == ";" {
			if p := next(); p != "" {
				return p
			}
			continue
		}
		end := strings.HasSuffix(w, ";")
		if w = strings.Trim(strings.TrimSuffix(w, ";"), "()'\"{}<>"); w != "" {
			part = append(part, w)
		}
		if end {
			if p := next(); p != "" {
				return p
			}
		}
	}
	if p := next(); p != "" {
		return p
	}
	return "a command"
}

func partProgram(words []string) string {
	for len(words) > 0 && keywords[words[0]] {
		words = words[1:]
	}
	if len(words) == 0 || words[0] == "test" || words[0] == "[" {
		return ""
	}
	for len(words) > 0 {
		w := words[0]
		name := path.Base(w)
		n, wrapper := positional[name]
		switch {
		case wrapper:
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				if slices.Contains(valueOpts[name], words[0]) && len(words) > 1 {
					words = words[1:]
				}
				words = words[1:]
			}
			words = words[min(n, len(words)):]
		case strings.HasPrefix(w, "-"), strings.Contains(w, "=") && !strings.HasPrefix(w, "/") && !strings.HasPrefix(w, "."):
			words = words[1:]
		default:
			if _, err := strconv.Atoi(w); err == nil {
				words = words[1:]
				continue
			}
			return name
		}
	}
	return ""
}

// Fires returns every time the job starts from from up to, but not
// including, to, read in the zone cron runs in.
func (j Job) Fires(from, to time.Time, zone *time.Location) []time.Time {
	var out []time.Time
	t := from.In(zone).Truncate(time.Minute)
	if t.Before(from) {
		t = t.Add(time.Minute)
	}
	for ; t.Before(to); t = t.Add(time.Minute) {
		if j.StartsAt(t) {
			out = append(out, t)
		}
	}
	return out
}

// StartsAt reports whether cron starts the job at t, a whole minute in the
// zone cron runs in. It applies cron's rule for the two day fields: when
// either starts with "*" both must match, and otherwise either one is
// enough.
func (j Job) StartsAt(t time.Time) bool {
	if j.fields == nil {
		return false
	}
	w := j.when
	if !w.minute.has(t.Minute()) || !w.hour.has(t.Hour()) || !w.month.has(int(t.Month())) {
		return false
	}
	dom, dow := w.dom.has(t.Day()), w.dow.has(int(t.Weekday()))
	if w.dom.star || w.dow.star {
		return dom && dow
	}
	return dom || dow
}

// Later rewrites the job's schedule to start minutes later, when its minute
// and hour are plain numbers, or its minute is and it runs every hour. A
// move past midnight is written only for a job that runs every day, since
// it would change the days of any other. It reports false for a schedule it
// can't rewrite.
func (j Job) Later(minutes int) (string, bool) {
	if j.fields == nil || minutes <= 0 {
		return "", false
	}
	f := slices.Clone(j.fields)
	m, err := strconv.Atoi(f[0])
	if err != nil {
		return "", false
	}
	everyDay := f[2] == "*" && f[3] == "*" && f[4] == "*"
	if f[1] == "*" {
		if m+minutes >= 60 {
			return "", false
		}
		f[0] = strconv.Itoa(m + minutes)
		return strings.Join(f, " "), true
	}
	h, err := strconv.Atoi(f[1])
	if err != nil {
		return "", false
	}
	at := h*60 + m + minutes
	if at >= 24*60 && !everyDay {
		return "", false
	}
	at %= 24 * 60
	f[0], f[1] = strconv.Itoa(at%60), strconv.Itoa(at/60)
	return strings.Join(f, " "), true
}

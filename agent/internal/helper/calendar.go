package helper

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// systemctl show prints each calendar trigger of a timer in systemd's
// normalised form, such as
// "{ OnCalendar=*-*-* 02:00:00 ; next_elapse=Sun 2026-11-01 02:00:00 UTC }",
// where "daily" has already become "*-*-* 00:00:00". The next run is in the
// time zone systemctl runs in, UTC for the helper, and reads "(null)" for a
// calendar that never comes again.

// calendarTrigger reads one TimersCalendar value: its OnCalendar expression,
// and when it next comes, or "" when it never does.
func calendarTrigger(v string) (spec, next string, ok bool) {
	_, rest, found := strings.Cut(v, "OnCalendar=")
	if !found {
		return "", "", false
	}
	spec, rest, _ = strings.Cut(rest, " ;")
	spec = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(spec), "}"))
	if _, n, found := strings.Cut(rest, "next_elapse="); found {
		next = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(n), "}"))
	}
	if next == "(null)" || next == "n/a" {
		next = ""
	}
	return spec, next, spec != ""
}

// shiftCalendar moves a normalised calendar expression minutes earlier. The
// time must have a fixed minute and second, and a fixed hour, a list of
// hours or "*". A calendar that fires every day may move back past
// midnight, and one that fires every hour past the hour; any other may not,
// since that would change the days or hours it fires on. Weekdays, the
// date and a time zone are kept as they are.
func shiftCalendar(spec string, minutes int) (string, error) {
	if minutes < 1 || minutes > 24*60 {
		return "", fmt.Errorf("a move of %d minutes is out of range", minutes)
	}
	fields := strings.Fields(spec)
	at := -1
	for i, f := range fields {
		if strings.Contains(f, ":") {
			if at >= 0 {
				return "", errors.New("it names more than one time")
			}
			at = i
		}
	}
	if at < 0 {
		return "", errors.New("it names no time of day")
	}
	parts := strings.Split(fields[at], ":")
	if len(parts) != 3 || !digits(parts[1]) || !digits(parts[2]) {
		return "", errors.New("its minutes and seconds are not single fixed values")
	}
	minute, _ := strconv.Atoi(parts[1])
	second, _ := strconv.Atoi(parts[2])
	if minute > 59 || second > 59 {
		return "", errors.New("it is not a time of day")
	}
	// Every day: the time is the second field, after a date of "*-*-*" and
	// no weekdays.
	daily := at == 1 && fields[0] == "*-*-*"
	switch hours := parts[0]; {
	case digits(hours):
		h, _ := strconv.Atoi(hours)
		total := h*60 + minute - minutes
		switch {
		case h > 23:
			return "", errors.New("it is not a time of day")
		case total < 0 && !daily:
			return "", errors.New("the move would carry it back past midnight, onto other days")
		}
		total = (total + 24*60) % (24 * 60)
		parts[0], parts[1] = fmt.Sprintf("%02d", total/60), fmt.Sprintf("%02d", total%60)
	case hours == "*":
		switch {
		case minutes >= 60:
			return "", errors.New("it fires every hour, so it can move less than an hour")
		case minute < minutes && !daily:
			return "", errors.New("the move would carry it back past the hour, onto other days")
		}
		parts[1] = fmt.Sprintf("%02d", (minute-minutes+60)%60)
	case allDigits(strings.Split(hours, ",")):
		if minute < minutes {
			return "", errors.New("the move would carry it back past the hour, onto other hours")
		}
		parts[1] = fmt.Sprintf("%02d", minute-minutes)
	default:
		return "", errors.New("its hours are not a fixed value, a list or *")
	}
	fields[at] = strings.Join(parts, ":")
	return strings.Join(fields, " "), nil
}

func digits(s string) bool {
	if s == "" || len(s) > 2 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func allDigits(ss []string) bool {
	for _, s := range ss {
		if !digits(s) {
			return false
		}
	}
	return len(ss) > 0
}

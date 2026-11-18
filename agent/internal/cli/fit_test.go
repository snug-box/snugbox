package cli

import (
	"strings"
	"testing"

	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

func TestDescribeNamesTheOOMDaemon(t *testing.T) {
	for daemon, want := range map[helperv1.OOMDaemon]string{
		helperv1.OOMDaemon_OOM_DAEMON_UNSPECIFIED:  "Turns on systemd-oomd",
		helperv1.OOMDaemon_OOM_DAEMON_SYSTEMD_OOMD: "Turns on systemd-oomd",
		helperv1.OOMDaemon_OOM_DAEMON_EARLYOOM:     "Turns on earlyoom, which stops the largest process",
	} {
		lines := describe(&helperv1.Fix{Kind: &helperv1.Fix_OomProtection{OomProtection: &helperv1.OOMProtectionFix{Daemon: daemon}}}, nil)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], want) {
			t.Errorf("%v: %q", daemon, lines)
		}
	}
}

func TestDescribeSchedule(t *testing.T) {
	f := &helperv1.Fix{Kind: &helperv1.Fix_Schedule{Schedule: &helperv1.ScheduleFix{Shifts: []*helperv1.TimerShift{
		{Timer: "dump.timer", Service: "dump.service", Minutes: 20}, {Timer: "index.timer", Service: "index.service", Minutes: 1},
	}}}}
	want := []string{
		"dump.timer: starts dump.service 20 minutes earlier, with a drop-in that moves its OnCalendar.",
		"index.timer: starts index.service 1 minute earlier, with a drop-in that moves its OnCalendar.",
	}
	if got := describe(f, nil); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("describe = %q, want %q", got, want)
	}
}

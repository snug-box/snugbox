package helperapi

import helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"

// stateNames are the words the journal, the local API and the command line
// use for each state of blueprint section 5.6.
var stateNames = map[helperv1.State]string{
	helperv1.State_STATE_APPROVED:  "approved",
	helperv1.State_STATE_PREFLIGHT: "preflight",
	helperv1.State_STATE_SNAPSHOT:  "snapshot",
	helperv1.State_STATE_APPLYING:  "applying",
	helperv1.State_STATE_VERIFYING: "verifying",
	helperv1.State_STATE_WATCHING:  "watching",
	helperv1.State_STATE_DONE:      "done",
	helperv1.State_STATE_REOPENED:  "reopened",
	helperv1.State_STATE_UNDOING:   "undoing",
	helperv1.State_STATE_UNDONE:    "undone",
	helperv1.State_STATE_FAILED:    "failed",
}

// StateName is the word for a state, or "" for none.
func StateName(s helperv1.State) string { return stateNames[s] }

// StateFromName reads a state word back, or STATE_UNSPECIFIED for an
// unknown one.
func StateFromName(name string) helperv1.State {
	for s, n := range stateNames {
		if n == name {
			return s
		}
	}
	return helperv1.State_STATE_UNSPECIFIED
}

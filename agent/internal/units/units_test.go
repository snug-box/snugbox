package units

import (
	"slices"
	"strings"
	"testing"
)

// settings parses a unit's Key=value lines; a key set twice keeps both.
func settings(unit string) map[string][]string {
	out := map[string][]string{}
	for line := range strings.Lines(unit) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '[' {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		out[k] = append(out[k], v)
	}
	return out
}

func TestRender(t *testing.T) {
	w, err := Watcher("/usr/local/bin/")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(string(w))["ExecStart"]; len(got) != 1 || got[0] != "/usr/local/bin/snugboxd" {
		t.Errorf("watcher ExecStart %q", got)
	}
	h, err := Helper("/usr/libexec/snugbox/snugbox-helper")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(string(h))["ExecStart"]; len(got) != 1 || got[0] != "/usr/libexec/snugbox/snugbox-helper" {
		t.Errorf("helper ExecStart %q", got)
	}
	for _, bad := range []string{"bin", "./bin", ""} {
		if _, err := Watcher(bad); err == nil {
			t.Errorf("Watcher(%q) rendered", bad)
		}
		if _, err := Helper(bad); err == nil {
			t.Errorf("Helper(%q) rendered", bad)
		}
	}
}

// Blueprint section 5.4: the socket is 0660, owned by snugbox:snugbox,
// inside a 0750 folder, with the owner set explicitly.
func TestHelperSocket(t *testing.T) {
	s := settings(string(Socket()))
	for k, want := range map[string]string{
		"ListenStream": "/run/snugbox/helper.sock", "SocketUser": "snugbox", "SocketGroup": "snugbox",
		"SocketMode": "0660", "DirectoryMode": "0750", "PassCredentials": "yes",
	} {
		if got := s[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := s["Accept"]; ok {
		t.Error("the helper takes the listening socket itself; Accept= must stay unset")
	}
}

func TestHelperSandbox(t *testing.T) {
	h, err := Helper("/usr/libexec/snugbox/snugbox-helper")
	if err != nil {
		t.Fatal(err)
	}
	s := settings(string(h))
	for k, want := range map[string]string{
		"RestrictAddressFamilies": "AF_UNIX", "ProtectHome": "read-only", "PrivateTmp": "yes",
		"NoNewPrivileges": "yes", "SystemCallFilter": "@system-service", "ProtectSystem": "strict",
		"CapabilityBoundingSet": "CAP_SYSLOG CAP_DAC_READ_SEARCH", "StateDirectoryMode": "0700",
	} {
		if got := s[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := s["User"]; ok {
		t.Error("the helper runs as root; User= must stay unset")
	}
	w, err := Watcher("/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(string(w))["RuntimeDirectoryPreserve"]; len(got) != 1 || got[0] != "yes" {
		t.Errorf("snugboxd must keep /run/snugbox when it stops, since the helper's socket lives there; RuntimeDirectoryPreserve = %q", got)
	}
}

// The settings file ships with every setting commented out, so installing it
// changes nothing, and the watcher's unit reads it from SettingsFile.
func TestSettingsFile(t *testing.T) {
	var keys []string
	for line := range strings.Lines(string(Settings())) {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case line[0] != '#':
			t.Errorf("%q is not commented out", line)
		default:
			if k, _, ok := strings.Cut(line[1:], "="); ok && strings.HasPrefix(k, "SNUGBOX_") {
				keys = append(keys, k)
			}
		}
	}
	if want := []string{"SNUGBOX_WEBHOOK_URL", "SNUGBOX_OFFLINE"}; !slices.Equal(keys, want) {
		t.Errorf("the settings file offers %q, want %q", keys, want)
	}
	w, err := Watcher("/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(string(w))["EnvironmentFile"]; len(got) != 1 || got[0] != "-"+SettingsFile {
		t.Errorf("snugboxd's EnvironmentFile = %q, want -%s", got, SettingsFile)
	}
}

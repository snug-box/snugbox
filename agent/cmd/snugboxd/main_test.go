package main

import "testing"

func TestOfflineSetting(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "0": false, "false": false, "No": false, "off": false,
		"1": true, "true": true, "YES": true, " on ": true,
	} {
		if got, warning := offlineSetting(v); got != want || warning != "" {
			t.Errorf("offlineSetting(%q) = %v, %q; want %v and no warning", v, got, warning, want)
		}
	}
	if got, warning := offlineSetting("ture"); !got || warning == "" {
		t.Errorf(`offlineSetting("ture") = %v, %q; want offline and a warning`, got, warning)
	}
}

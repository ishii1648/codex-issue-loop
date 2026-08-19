package capability

import (
	"reflect"
	"testing"
)

func block(profile, network string, browser, download, timeGate bool) string {
	return "<!-- agent-loop:capabilities\n" +
		"version: 1\nprofile: " + profile + "\nnetwork: " + network +
		"\nbrowser_cdp: " + boolText(browser) + "\ndownload: " + boolText(download) +
		"\nexternal_time_gate: " + boolText(timeGate) + "\n-->"
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestCapabilityCompatibilityTable(t *testing.T) {
	profiles := map[string]Provider{
		"standard": {Version: 1, Profile: "standard", Network: NetworkNone},
		"extended": {Version: 1, Profile: "extended", Network: NetworkLocalhost, BrowserCDP: true, Download: true, ExternalTimeGate: true},
	}
	tests := []struct {
		name string
		body string
		want bool
		code string
	}{
		{name: "none positive", body: block("standard", "none", false, false, false), want: true},
		{name: "localhost positive", body: block("extended", "localhost", false, false, false), want: true},
		{name: "public network negative", body: block("extended", "public", false, false, false), code: CodeNetworkMismatch},
		{name: "browser positive", body: block("extended", "localhost", true, false, false), want: true},
		{name: "browser negative", body: block("standard", "none", true, false, false), code: CodeBrowserCDPMismatch},
		{name: "download positive", body: block("extended", "localhost", false, true, false), want: true},
		{name: "download negative", body: block("standard", "none", false, true, false), code: CodeDownloadMismatch},
		{name: "external time positive", body: block("extended", "none", false, false, true), want: true},
		{name: "external time negative", body: block("standard", "none", false, false, true), code: CodeExternalTimeMismatch},
		{name: "missing fails closed", body: "", code: CodeMetadataMissing},
		{name: "unknown fails closed", body: block("standard", "none", false, false, false) + "\n<!-- agent-loop:capabilities\nversion: 1\nunknown: true\n-->", code: CodeMetadataInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Evaluate(test.body, profiles)
			if got.Compatible != test.want {
				t.Fatalf("compatible=%v mismatches=%+v", got.Compatible, got.Mismatches)
			}
			if test.code != "" {
				codes := []string{}
				for _, mismatch := range got.Mismatches {
					codes = append(codes, mismatch.Code)
				}
				if !contains(codes, test.code) {
					t.Fatalf("codes=%v want %s", codes, test.code)
				}
			}
		})
	}
}

func TestEvaluationIsDeterministicAndContainsNoCredentialMaterial(t *testing.T) {
	body := block("extended", "public", true, true, true)
	profiles := map[string]Provider{"extended": {Version: 1, Profile: "extended", Network: NetworkNone}}
	left, right := Evaluate(body, profiles), Evaluate(body, profiles)
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("evaluation changed: left=%+v right=%+v", left, right)
	}
	for _, mismatch := range left.Mismatches {
		if mismatch.Field == "secret" || mismatch.Field == "credential" || mismatch.Field == "token" {
			t.Fatalf("secret-bearing mismatch field: %+v", mismatch)
		}
	}
}

func TestPersistedRequirementUsesSameFailClosedPredicateAfterProfileChange(t *testing.T) {
	requirement, mismatches := Parse(block("extended", "localhost", true, true, false))
	if requirement == nil || len(mismatches) != 0 {
		t.Fatalf("parse=%+v mismatches=%+v", requirement, mismatches)
	}
	evaluation := EvaluateRequirement(requirement, map[string]Provider{
		"extended": {Version: 1, Profile: "extended", Network: NetworkNone},
	})
	if evaluation.Compatible || !contains([]string{
		evaluation.Mismatches[0].Code,
	}, CodeNetworkMismatch) {
		t.Fatalf("profile drift was admitted: %+v", evaluation)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

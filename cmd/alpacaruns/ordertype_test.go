package main

// ordertype_test.go: every order type the CLI can produce, and every
// combination it must refuse with a sentence instead of a broker 422.

import "testing"

func TestResolveOrderType(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		limit    string
		stop     string
		isOption bool
		want     string
		wantErr  bool
	}{
		{"default is market", "", "", "", false, "market", false},
		{"limit price implies limit", "", "100", "", false, "limit", false},
		{"explicit market", "market", "", "", false, "market", false},
		{"buy limit", "limit", "100", "", false, "limit", false},
		{"sell limit", "limit", "100", "", true, "limit", false},
		{"stop", "stop", "", "95", false, "stop", false},
		{"stop limit", "stop_limit", "94", "95", false, "stop_limit", false},
		{"market with a price is a mistake", "market", "100", "", false, "", true},
		{"limit without a price", "limit", "", "", false, "", true},
		{"limit with a stop", "limit", "100", "95", false, "", true},
		{"stop without a trigger", "stop", "", "", false, "", true},
		{"stop with a limit", "stop", "94", "95", false, "", true},
		{"stop_limit missing one leg", "stop_limit", "94", "", false, "", true},
		{"stop_limit missing the stop", "stop_limit", "", "95", false, "", true},
		{"negative price", "limit", "-5", "", false, "", true},
		{"non-numeric stop", "stop", "", "abc", false, "", true},
		{"unknown type", "iceberg", "", "", false, "", true},
		{"options: market", "market", "", "", true, "market", false},
		{"options: limit", "limit", "3.5", "", true, "limit", false},
		{"options: stop rejected", "stop", "", "3", true, "", true},
		{"options: stop_limit rejected", "stop_limit", "2.5", "3", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOrderType(tc.kind, tc.limit, tc.stop, tc.isOption)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %q/%q/%q (option=%v) as %q", tc.kind, tc.limit, tc.stop, tc.isOption, got)
				}
				if err.Error() == "" {
					t.Fatal("refusal carried no message")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected a valid combination: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

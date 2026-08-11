package app

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseIDList(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"csv", "id1,id2", []string{"id1", "id2"}, false},
		{"csv with spaces", " id1 , id2 ", []string{"id1", "id2"}, false},
		{"csv single", "abc", []string{"abc"}, false},
		{"csv single padded", "  184fd1d7742ac942  ", []string{"184fd1d7742ac942"}, false},
		{"csv trailing comma", "id1,id2,", []string{"id1", "id2"}, false},
		{"csv empty items dropped", "id1,,id2", []string{"id1", "id2"}, false},
		{"empty string", "", nil, true},
		{"whitespace only", "   ", nil, true},
		{"only commas", ",,,", nil, true},
		{"json array rejected", `["id1","id2"]`, nil, true},
		{"json array padded rejected", ` [ "id1" ] `, nil, true},
		{"empty json array rejected", `[]`, nil, true},
		{"malformed json rejected", `["id1",`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseIDList(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseIDList(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIDList(%q) unexpected error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseIDList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// A JSON-array input must fail with the pointed "no longer accepts" error, not
// a generic parse failure — the message is the migration hint.
func TestParseIDListJSONRejectionMessage(t *testing.T) {
	_, err := parseIDList(`["id1","id2"]`)
	if err == nil {
		t.Fatal("expected error for JSON array input")
	}
	if !strings.Contains(err.Error(), "no longer accepts a JSON array") {
		t.Fatalf("error should name the removed JSON-array format, got: %v", err)
	}
}

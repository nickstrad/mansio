package outcome

import (
	"strings"
	"testing"
)

func TestEveryCodeRoundTripsThroughItsName(t *testing.T) {
	for _, c := range Codes() {
		got, ok := Parse(c.String())
		if !ok || got != c {
			t.Fatalf("Parse(%q) = %v, %v; want %v", c.String(), got, ok, c)
		}
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Code
		ok   bool
	}{
		{"ok", OK, true},
		{"STMT_TIMEOUT", StmtTimeout, true}, // case-insensitive
		{"error", Error, true},
		{"unavailable", Unavailable, true}, // last code: on point (a wire code, RI-10)
		{"", 0, false},
		{"any", 0, false}, // "any" is a set name, not a code
		{"timeout", 0, false},
	}
	for _, tc := range tests {
		got, ok := Parse(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Parse(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestStringOfUnknownCode(t *testing.T) {
	if got := numCodes.String(); got != "outcome(10)" {
		t.Fatalf("String() = %q", got)
	}
}

func TestParseSet(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		has     []Code
		hasNot  []Code
		wantErr bool
	}{
		{"empty", nil, nil, Codes(), false},
		{"one", []string{"shed"}, []Code{Shed}, []Code{OK, Aborted}, false},
		{"any covers failures but not ok", []string{"any"}, []Code{Shed, StmtTimeout, Error, GenFull}, []Code{OK}, false},
		{"unknown name", []string{"shed", "nope"}, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSet(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			for _, c := range tc.has {
				if !s.Has(c) {
					t.Errorf("set lacks %v", c)
				}
			}
			for _, c := range tc.hasNot {
				if s.Has(c) {
					t.Errorf("set has %v", c)
				}
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, c := range Codes() {
		f.Add(c.String())
	}
	f.Add("any")
	f.Fuzz(func(t *testing.T, s string) {
		c, ok := Parse(s)
		if ok && !strings.EqualFold(c.String(), s) {
			t.Fatalf("Parse(%q) = %v, whose name differs", s, c)
		}
		if !ok && c != 0 {
			t.Fatalf("failed Parse returned %v", c)
		}
	})
}

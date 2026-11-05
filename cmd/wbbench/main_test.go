package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRealMainExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"-sizes", "0"}, exitUsage},
		{[]string{"-sizes", "1X"}, exitUsage},
		{[]string{"-reps", "0"}, exitUsage},
		{[]string{"extra"}, exitUsage},
		{[]string{"-sizes", "8K", "-reps", "1", "-tick-dbs", "2"}, exitOK},
	} {
		var out, errb bytes.Buffer
		if got := realMain(context.Background(), tc.args, &out, &errb); got != tc.want {
			t.Errorf("%v: exit %d, want %d (stderr %s)", tc.args, got, tc.want, errb.String())
		}
		if tc.want == exitOK && !strings.Contains(out.String(), "dirty check") {
			t.Errorf("%v: output %q", tc.args, out.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	if got := realMain(ctx, []string{"-sizes", "8K", "-tick-dbs", "1"}, &out, &errb); got != exitInterrupted {
		t.Errorf("cancelled: exit %d, want %d (%s)", got, exitInterrupted, errb.String())
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "1K": 1024, "64K": 65536, "2M": 2 << 20, "1G": 1 << 30} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-1", "K", "1.5M"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}

package capacity_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/capacity"
)

func TestSectionAndCheck(t *testing.T) {
	rows := []capacity.Row{{Queue: "q", Protects: "Postgres", Capacity: "2 (`x`)", Workers: "2 slots", Policy: "shed 503", Deadline: "the request's",
		Metric: "`mansio_queue_*{queue=\"q\"}`", Lever: capacity.NoLever, FirstLook: "the store's latency"}}
	sec := capacity.Section("svc", "team", rows)
	for _, want := range []string{"| `q`  ", "| Postgres |", "#### svc: `q`", "- Downstream dependency: Postgres", "- Rollback or feature flag: none (config, restart)"} {
		if !strings.Contains(sec, want) {
			t.Errorf("section lacks %q:\n%s", want, sec)
		}
	}
	path := filepath.Join(t.TempDir(), "doc.md")
	os.WriteFile(path, []byte("intro\n<!-- capacity:svc:begin -->\nold\n<!-- capacity:svc:end -->\ntail\n"), 0o644)
	if err := capacity.Check(path, "svc", sec, false); err == nil {
		t.Fatal("a stale section passed")
	}
	if err := capacity.Check(path, "other", sec, true); err == nil {
		t.Fatal("a missing section's markers were not reported")
	}
	if err := capacity.Check(path, "svc", sec, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "intro\n<!-- capacity:svc:begin -->\n"+sec+"<!-- capacity:svc:end -->\ntail\n" {
		t.Fatalf("after update:\n%s", got)
	}
	if err := capacity.Check(path, "svc", sec, false); err != nil {
		t.Fatalf("a current section failed: %v", err)
	}
}

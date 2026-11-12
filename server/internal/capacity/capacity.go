// Package capacity renders the capacity table (docs/capacity.md): every bounded queue and slot pool on a request's
// path, with its capacity, workers, full-queue policy, the deadline it lives under, its metric and its runtime lever,
// so an operator can review capacity and act on a full queue from one page. Each daemon lists its own rows from its
// config defaults (a capacity_test in cmd/agentd, cmd/controld and cmd/edge) and owns one section of the document;
// the tests fail when the document and the defaults disagree, and rewrite their section with -update.
//
// Error module: none (test and documentation support; no I/O besides the document).
package capacity

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Row is one bounded queue or slot pool.
type Row struct {
	Queue    string // its queue label in mansio_queue_* (or its own metric's name when it has no such row)
	Protects string // the downstream dependency or local resource the bound protects
	Capacity string // the default, with the config key that sets it
	Workers  string // what drains it: workers or slots in flight
	Policy   string // what a caller sees when it is full
	Deadline string // the deadline work in it lives under
	Metric   string // the series that show depth, capacity and refusals
	Lever    string // how to change it at run time, or NoLever
	// The rest fills the runbook: what to look at first when it is full, and what full is a symptom of.
	FirstLook string
}

// NoLever is Row.Lever for a bound that only the config file changes.
const NoLever = "none (config, restart)"

// Section renders one daemon's rows: the table, then one runbook per row in the book's template.
func Section(service, owner string, rows []Row) string {
	head := []string{"Queue", "Protects", "Capacity (config)", "Workers", "When full", "Lives under", "Metric", "Runtime lever"}
	cells := [][]string{head}
	for _, r := range rows {
		cells = append(cells, []string{"`" + r.Queue + "`", r.Protects, r.Capacity, r.Workers, r.Policy, r.Deadline, r.Metric, r.Lever})
	}
	width := make([]int, len(head))
	for _, c := range cells {
		for i, s := range c {
			width[i] = max(width[i], len([]rune(s)))
		}
	}
	var b strings.Builder
	line := func(c []string, pad string) {
		for i, s := range c {
			b.WriteString("| " + s + strings.Repeat(pad, width[i]-len([]rune(s))) + " ")
		}
		b.WriteString("|\n")
	}
	line(head, " ")
	dashes := make([]string, len(head))
	for i := range dashes {
		dashes[i] = strings.Repeat("-", width[i])
	}
	line(dashes, "-")
	for _, c := range cells[1:] {
		line(c, " ")
	}
	fmt.Fprintf(&b, "\n### Runbooks: %s\n", service)
	for _, r := range rows {
		fmt.Fprintf(&b, `
#### %s: `+"`%s`"+`

- Service: %s
- Queue name: `+"`%s`"+`
- Owner: %s
- Policy: %s
- Capacity: %s
- Workers: %s
- Downstream dependency: %s
- Expected full behavior: %s Work in it lives under: %s.
- Primary dashboard: %s
- Rollback or feature flag: %s
- First look: %s
`, service, r.Queue, service, r.Queue, owner, r.Policy, r.Capacity, r.Workers, r.Protects, r.Policy, r.Deadline, r.Metric, r.Lever, r.FirstLook)
	}
	return b.String()
}

// Check compares the section named name in the document at path (the text between its begin and end marker
// lines) with want; with update it rewrites the section instead. The markers are
// "<!-- capacity:<name>:begin -->" and "<!-- capacity:<name>:end -->", each on its own line.
func Check(path, name, want string, update bool) error {
	doc, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	begin, end := []byte("<!-- capacity:"+name+":begin -->\n"), []byte("<!-- capacity:"+name+":end -->\n")
	i, j := bytes.Index(doc, begin), bytes.Index(doc, end)
	if i < 0 || j < i {
		return fmt.Errorf("%s has no %q section markers", path, name)
	}
	got := doc[i+len(begin) : j]
	if string(got) == want {
		return nil
	}
	if !update {
		return fmt.Errorf("%s: the %s section is not what the config defaults generate; run this test with -update", path, name)
	}
	out := append(append(append([]byte{}, doc[:i+len(begin)]...), want...), doc[j:]...)
	return os.WriteFile(path, out, 0o644)
}

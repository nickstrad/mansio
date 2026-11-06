package load

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/outcome"
	"github.com/nickstrad/ts-db-go/internal/retry"
	"github.com/nickstrad/ts-db-go/internal/workload"
)

func parse(t *testing.T, body string) *Config {
	t.Helper()
	c, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return c
}

const onePhase = `"phases":[{"dbs":1,"duration":"1s","start_qps":1}]`

func TestShippedConfigsLoad(t *testing.T) {
	files, _ := filepath.Glob("../../configs/*.json")
	if len(files) == 0 {
		t.Fatal("no configs found")
	}
	for _, f := range files {
		if _, err := LoadFile(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestLoadFileWrapsErrors(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("got %v, want an error naming the file", err)
	}
}

func TestDefaultsApplyOnlyWhereTheFileIsSilent(t *testing.T) {
	c := parse(t, `{"workers":7,"schema":{"max_rows":900},"workload":{"bulk_max":9},`+onePhase+`}`)
	d := Defaults()
	if c.Workers != 7 || c.StatementTimeout != d.StatementTimeout || c.Seed != 1 || c.RecordFraction != 1 {
		t.Fatalf("scalar defaults: %+v", c)
	}
	if c.Schema.MaxRows != 900 || c.Schema.MinRows != d.Schema.MinRows || c.Schema.IndexProb != d.Schema.IndexProb {
		t.Fatalf("schema section did not overlay defaults: %+v", c.Schema)
	}
	if c.Workload.BulkMax != 9 || c.Workload.BulkMin != d.Workload.BulkMin {
		t.Fatalf("workload section did not overlay defaults: %+v", c.Workload)
	}
	if len(c.Mix) != len(workload.DefaultMix()) || len(c.Clients) != 1 || c.Clients[0].Name != "default" {
		t.Fatalf("mix/clients defaults: %v %v", c.Mix, c.Clients)
	}
}

// A mix in the file replaces the default mix; it does not merge into it.
func TestMixReplacesTheDefault(t *testing.T) {
	c := parse(t, `{"mix":{"point_read":1},`+onePhase+`}`)
	if len(c.Mix) != 1 || len(c.Phases[0].Mix) != 1 {
		t.Fatalf("mix = %v", c.Mix)
	}
}

func TestPhaseInheritance(t *testing.T) {
	c := parse(t, `{"mix":{"point_read":1},"distribution":{"kind":"zipf"},
		"clients":[{"name":"a","share":1},{"name":"b","share":3}],
		"phases":[{"dbs":5,"duration":"1s","start_qps":5},
		          {"dbs":6,"duration":"1s","start_qps":5,"mix":{"insert":1},"client_shares":{"b":1},"distribution":{"kind":"uniform"}}]}`)
	p0, p1 := c.Phases[0], c.Phases[1]
	if p0.Distribution.Kind != Zipf || p0.Distribution.S != 1.1 || p1.Distribution.Kind != Uniform {
		t.Fatalf("distribution: %+v %+v", p0.Distribution, p1.Distribution)
	}
	if p0.Mix[workload.PointRead] != 1 || p1.Mix[workload.Insert] != 1 || len(p1.Mix) != 1 {
		t.Fatal("mix inheritance")
	}
	if p0.ClientShares["b"] != 3 || p1.ClientShares["a"] != 0 {
		t.Fatalf("client shares: %v %v", p0.ClientShares, p1.ClientShares)
	}
	if p0.Shape != Linear || p0.Period != p0.Duration || p0.Name != "phase-0" {
		t.Fatalf("phase defaults: %+v", p0)
	}
}

func TestRates(t *testing.T) {
	tests := []struct {
		name       string
		phase      string
		start, end float64
	}{
		{"end defaults to start", `{"dbs":1,"duration":"1s","start_qps":5}`, 5, 5},
		{"explicit end", `{"dbs":1,"duration":"1s","start_qps":5,"end_qps":9}`, 5, 9},
		{"qpm converts", `{"dbs":1,"duration":"1s","start_qpm":600,"end_qpm":1200}`, 10, 20},
		{"qpm end defaults to start", `{"dbs":1,"duration":"1s","start_qpm":120}`, 2, 2},
		{"ramp down to zero via qpm", `{"dbs":1,"duration":"1s","start_qpm":0,"end_qpm":60}`, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := parse(t, `{"phases":[`+tc.phase+`]}`).Phases[0]
			if p.StartQPS != tc.start || p.EndQPS != tc.end {
				t.Fatalf("got %v→%v, want %v→%v", p.StartQPS, p.EndQPS, tc.start, tc.end)
			}
		})
	}
}

func TestClientPoliciesAreBuiltOnce(t *testing.T) {
	c := parse(t, `{"clients":[{"name":"good","share":1,"retry":{"max_attempts":3,"backoff":"exponential","jitter":"full","retry_on":["shed","expired"]}},
		{"name":"bad","share":1,"retry":{"max_attempts":9,"retry_on":["any"]}},{"name":"plain","share":1}],`+onePhase+`}`)
	good, bad, plain := c.Clients[0].Policy(), c.Clients[1].Policy(), c.Clients[2].Policy()
	if good.MaxAttempts != 3 || good.Backoff != retry.Exponential || good.Base != retry.DefaultBase ||
		good.RetryOn != outcome.SetOf(outcome.Shed, outcome.Expired) {
		t.Fatalf("good: %+v", good)
	}
	if bad.RetryOn != outcome.All || bad.Backoff != retry.Exponential || bad.Jitter != retry.Full {
		t.Fatalf("bad: %+v; a profile that leaves backoff and jitter out gets the safe defaults", bad)
	}
	if plain.MaxAttempts != 1 || plain.RetryOn != outcome.SetOf(outcome.Shed, outcome.Unavailable, outcome.Aborted) {
		t.Fatalf("plain: %+v", plain)
	}
	if got := c.ClientNames(); strings.Join(got, ",") != "good,bad,plain" {
		t.Fatalf("ClientNames = %v", got)
	}
}

func TestActorSectionAndWarnings(t *testing.T) {
	c := parse(t, `{"actor":{"journal_mode":"delete","read_conns":2},`+onePhase+`}`)
	if a := c.Actor.Actor(); a.JournalMode != "DELETE" || a.Synchronous != "NORMAL" || a.ReadConns != 2 {
		t.Fatalf("%+v", a)
	}
	if len(c.Warnings()) != 1 {
		t.Fatalf("want a read_conns warning, got %v", c.Warnings())
	}
	d := parse(t, `{`+onePhase+`}`)
	if d.Actor.Actor().JournalMode != "WAL" || len(d.Warnings()) != 0 {
		t.Fatalf("defaults: %+v %v", d.Actor.Actor(), d.Warnings())
	}
}

func TestValidationErrors(t *testing.T) {
	ph := `"phases":[{"dbs":1,"duration":"1s"}]`
	cases := []struct{ body, want string }{
		{`{"phases":[]}`, "at least one phase"},
		{`{"phases":[{"dbs":0,"duration":"1s"}]}`, "dbs must be > 0"},
		{`{"phases":[{"dbs":1,"duration":"0s"}]}`, "duration must be > 0"},
		{`{"phases":[{"dbs":1,"duration":"1s","start_qps":1,"start_qpm":5}]}`, "not both"},
		{`{"phases":[{"dbs":1,"duration":"1s","start_qps":-1}]}`, "rates must be >= 0"},
		{`{"phases":[{"dbs":1,"duration":"1s","shape":"saw"}]}`, "shape"},
		{`{"phases":[{"dbs":1,"duration":"ten"}]}`, "invalid duration"},
		{`{"mix":{"nope":1},` + ph + `}`, "unknown name"},
		{`{"mix":{"insert":0},` + ph + `}`, "sum to > 0"},
		{`{"mix":{"insert":-1,"delete":2},` + ph + `}`, "negative"},
		{`{"typo_field":true,` + ph + `}`, "unknown field"},
		{`{"workers":0,` + ph + `}`, "workers"},
		{`{"record_fraction":0,` + ph + `}`, "record_fraction"},
		{`{"record_fraction":1.5,` + ph + `}`, "record_fraction"},
		{`{"actor":{"journal_mode":"wal2"},` + ph + `}`, "journal_mode"},
		{`{"actor":{"synchronous":"sometimes"},` + ph + `}`, "synchronous"},
		{`{"schema":{"min_tables":0},` + ph + `}`, "schema"},
		{`{"workload":{"bulk_min":9,"bulk_max":3},` + ph + `}`, "workload"},
		{`{"distribution":{"kind":"zipf","s":0.5},` + ph + `}`, "zipf s"},
		{`{"distribution":{"kind":"pareto"},` + ph + `}`, "kind"},
		{`{"clients":[{"name":"a","share":1,"retry":{"jitter":"lots"}}],` + ph + `}`, "jitter"},
		{`{"clients":[{"name":"a","share":1,"retry":{"retry_on":["oops"]}}],` + ph + `}`, "unknown outcome"},
		{`{"clients":[{"name":"a","share":1},{"name":"a","share":1}],` + ph + `}`, "duplicate"},
		{`{"clients":[{"name":"a,b","share":1}],` + ph + `}`, "CSV field"},
		{`{"clients":[{"name":"a","share":1,"retry":{"max_attempts":256}}],` + ph + `}`, "max_attempts"},
		{`{"clients":[{"name":"a","share":1}],"phases":[{"dbs":1,"duration":"1s","client_shares":{"b":1}}]}`, "client_shares"},
	}
	for _, tc := range cases {
		_, err := Parse(strings.NewReader(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.body, err, tc.want)
		}
	}
}

// Every problem is reported at once, not just the first.
func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"workers":0,"phases":[{"dbs":0,"duration":"1s"},{"dbs":1,"duration":"1s","shape":"saw"}]}`))
	for _, want := range []string{"workers", "phases[0]", "phases[1]"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
	}
}

func TestFormatWeightsIsSorted(t *testing.T) {
	if got := FormatWeights(map[string]float64{"b": 2, "a": 0.5}); got != "a=0.5 b=2" {
		t.Fatalf("got %q", got)
	}
}

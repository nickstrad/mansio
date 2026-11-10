package front

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateContract = flag.Bool("update", false, "rewrite ir/vectors/retry.json and docs/retry-contract.md from Contract()")

// The files generated from Contract, relative to this package.
const (
	retryVectorsFile = "../../../ir/vectors/retry.json"
	retryDocFile     = "../../../docs/retry-contract.md"
)

// retryVectors is ir/vectors/retry.json: the caller contract as shared test vectors. The Go client
// (internal/client) and the TypeScript SDK (tests/unit/sdk_retry_contract_test.ts) both run them.
func retryVectors() []byte {
	type vector struct {
		Name string `json:"name"`
		Why  string `json:"why"`
		ContractRow
	}
	file := struct {
		Category    string         `json:"category"`
		Version     int            `json:"version"`
		Description string         `json:"description"`
		Policy      ContractPolicy `json:"policy"`
		Vectors     []vector       `json:"vectors"`
	}{Category: "retry", Version: 1, Policy: DefaultContractPolicy,
		Description: "The caller contract: for each answer from an agent or from controld's resolve, what a client library does by itself. " +
			"Generated from mansio-server's front.Contract (go test ./internal/front -run TestContractFiles -update); do not edit."}
	for _, r := range Contract() {
		file.Vectors = append(file.Vectors, vector{Name: r.From + ": " + r.Code, Why: r.Meaning, ContractRow: r})
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(file); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// retryDoc is docs/retry-contract.md: the same rows as a table for people.
func retryDoc() []byte {
	p := DefaultContractPolicy
	head := []string{"From", "Code", "Status", "`Retry-After`", "Client does", "Retry budget", "A mutation may have applied", "Meaning"}
	rows := [][]string{head}
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	for _, r := range Contract() {
		status := "none (no answer)"
		if r.Status != 0 {
			status = fmt.Sprint(r.Status)
		}
		budget := map[string]string{BudgetNone: "not charged", BudgetEvery: "charged", BudgetAfterFirst: "charged after the first"}[r.Budget]
		rows = append(rows, []string{r.From, "`" + r.Code + "`", status, yes(r.RetryAfter), string(r.Action), budget, yes(r.MayHaveApplied), r.Meaning})
	}
	width := make([]int, len(head))
	for _, r := range rows {
		for i, c := range r {
			width[i] = max(width[i], len([]rune(c)))
		}
	}
	var b strings.Builder
	line := func(cells []string, pad string) {
		for i, c := range cells {
			b.WriteString("| " + c + strings.Repeat(pad, width[i]-len([]rune(c))) + " ")
		}
		b.WriteString("|\n")
	}
	fmt.Fprintf(&b, `# Retry contract

<!-- Generated from mansio-server's front.Contract. Do not edit: change server/internal/front and run -->
<!-- (cd server && go test ./internal/front -run TestContractFiles -update). -->

What a mansio client library does by itself after each answer. The TypeScript SDK and the Go client follow
this one table; both run it as test vectors ([`+"`ir/vectors/retry.json`"+`](../ir/vectors/retry.json)).

- **done**: the answer is returned.
- **retry**: the same request again on the same route, after the larger of `+"`Retry-After`"+` and a jittered
  backoff.
- **reroute**: the route is dropped, the database is resolved again (or a new ticket fetched), then the same
  request is sent again. The first reroute of a call waits a short jittered delay; later ones back off.
- **fail**: final. The caller gets the error and decides.

A mutation is always sent again with the same request id, so it applies at most once.

`)
	line(rows[0], " ")
	dashes := make([]string, len(head))
	for i := range dashes {
		dashes[i] = strings.Repeat("-", width[i])
	}
	line(dashes, "-")
	for _, r := range rows[1:] {
		line(r, " ")
	}
	fmt.Fprintf(&b, `
Limits, the same in both clients by default:

| Limit                                         | Default |
| --------------------------------------------- | ------- |
| Attempts of one request on one route          | %-7d |
| Reroutes of one call                          | %-7d |
| Backoff: full jitter under base × 2^(n-1)     | %-7s |
| Backoff cap                                   | %-7s |
| Longest `+"`Retry-After`"+` honored                 | %-7s |
| Retry budget: tokens earned per first attempt | %-7g |
| Retry budget: most tokens banked              | %-7d |

No wait ends past the call's deadline: a retry that would is not made. The budget is shared by every call of
one client, so a client facing an outage adds at most a tenth more load in retries once its banked tokens are
spent. `+"`expired`"+` is not retried by the library because the deadline it was given is already spent;
resubmitting it would add load at the moment the server is slowest.
`, p.MaxAttempts, p.MaxReroutes, fmt.Sprintf("%d ms", p.BackoffBaseMS), fmt.Sprintf("%d s", p.BackoffMaxMS/1000),
		fmt.Sprintf("%d s", p.MaxRetryAfterMS/1000), p.BudgetRatio, p.BudgetBurst)
	return []byte(b.String())
}

// The committed contract files are exactly what Contract generates (run with -update after changing it).
func TestContractFiles(t *testing.T) {
	for path, want := range map[string][]byte{retryVectorsFile: retryVectors(), retryDocFile: retryDoc()} {
		if *updateContract {
			if err := os.WriteFile(filepath.FromSlash(path), want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is not what front.Contract generates (%v); run: go test ./internal/front -run TestContractFiles -update", path, err)
		}
	}
}

// Every action in Spec is one of the four, the budget follows from it, and only answers after which a write
// may have run are marked so.
func TestContractRows(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Contract() {
		key := r.From + ":" + r.Code
		if seen[key] {
			t.Errorf("%s twice", key)
		}
		seen[key] = true
		wantBudget := map[ClientAction]string{ActDone: BudgetNone, ActFail: BudgetNone, ActRetry: BudgetEvery, ActReroute: BudgetAfterFirst}
		if b, ok := wantBudget[r.Action]; !ok || b != r.Budget {
			t.Errorf("%s: action %q with budget %q", key, r.Action, r.Budget)
		}
		if r.RetryAfter && r.Status != 503 && r.Code != string(Fenced) {
			t.Errorf("%s: Retry-After on a %d", key, r.Status)
		}
		if r.MayHaveApplied && r.From != "agent" {
			t.Errorf("%s: a resolve never applies a write", key)
		}
	}
	for _, c := range Codes {
		if !seen["agent:"+string(c)] {
			t.Errorf("no contract row for %s", c)
		}
	}
}

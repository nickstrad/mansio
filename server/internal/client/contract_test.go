package client

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/retry"
)

// B8: the client is held to the caller contract by the shared vectors (ir/vectors/retry.json, generated from
// front.Contract; the TypeScript SDK runs the same file). For each answer: the step the loop takes under the
// default policy, and whether a write that ends there may have applied.
func TestRetryContractVectors(t *testing.T) {
	raw, err := os.ReadFile("../../../ir/vectors/retry.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Policy  front.ContractPolicy
		Vectors []front.ContractRow
	}
	if err := json.Unmarshal(raw, &file); err != nil || len(file.Vectors) == 0 {
		t.Fatalf("retry.json: %v (%d vectors)", err, len(file.Vectors))
	}
	var policy retry.Policy
	if err := policy.Normalize(); err != nil {
		t.Fatal(err)
	}
	got := front.ContractPolicy{MaxAttempts: policy.MaxAttempts, MaxReroutes: DefaultMaxReroutes, BackoffBaseMS: int(policy.Base.Milliseconds()),
		BackoffMaxMS: int(policy.Max.Milliseconds()), MaxRetryAfterMS: int(retryAfter("3600", 0).Milliseconds()),
		BudgetRatio: DefaultBudgetRatio, BudgetBurst: retry.BudgetAllowance}
	if got != file.Policy {
		t.Errorf("default policy %+v, the contract's %+v", got, file.Policy)
	}
	for _, v := range file.Vectors {
		a := answer{Code: v.Code, Ambiguous: v.Code == string(front.Expired)}
		var st step
		want := map[front.ClientAction]step{front.ActDone: stepDone, front.ActRetry: stepRetry, front.ActReroute: stepReroute, front.ActFail: stepFail}[v.Action]
		if v.From == "resolve" {
			if v.Code == "ok" {
				continue // a route: there is no step to decide
			}
			st, _ = resolveStep(a)
		} else {
			s, o := decide(a)
			if st = s; s == stepRetry && !policy.RetryOn.Has(o) { // the policy refuses this outcome: the call fails
				st = stepFail
			}
			mayApply := a.Code == codeTransport || a.Ambiguous || a.Code == string(front.Internal) // client.go's MayHaveApplied
			if st != stepDone && mayApply != v.MayHaveApplied {
				t.Errorf("%s: a write may have applied = %v, contract says %v", v.Code, mayApply, v.MayHaveApplied)
			}
		}
		if st != want {
			t.Errorf("%s %s: step %d, contract says %s", v.From, v.Code, st, v.Action)
		}
		// Budget: a retry always takes a token (the loop's stepRetry branch); a reroute after the first does.
		if wantBudget := map[step]string{stepRetry: front.BudgetEvery, stepReroute: front.BudgetAfterFirst}[st]; wantBudget != v.Budget {
			t.Errorf("%s %s: the loop charges %q, contract says %q", v.From, v.Code, wantBudget, v.Budget)
		}
	}
}

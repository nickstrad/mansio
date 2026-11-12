package front

// The caller contract (Backpressure in Microservices ch. 2 "Caller Contract"): for every answer a client can
// get from an agent or from controld's resolve, what a client library does by itself. It is written once, here:
// Contract's rows are generated into docs/retry-contract.md and intermediate-representation/vectors/retry.json (contract_test.go), and
// the Go client (internal/client) and the TypeScript SDK (sdk/retry.ts) both run those vectors.

// ClientAction is what a client library does after an answer, without asking its caller.
type ClientAction string

const (
	ActDone    ClientAction = "done"    // return the answer
	ActRetry   ClientAction = "retry"   // the same route again, after max(Retry-After, jittered backoff)
	ActReroute ClientAction = "reroute" // drop the route, resolve again (or get a new ticket), then try again
	ActFail    ClientAction = "fail"    // final: report it to the caller
)

// How a step is charged to the client's retry budget.
const (
	BudgetNone       = ""            // not charged
	BudgetEvery      = "every"       // each one takes a token
	BudgetAfterFirst = "after_first" // a reroute after the call's first takes a token (the first is free)
)

// ContractRow is one answer and the client's part of the contract for it.
type ContractRow struct {
	From       string       `json:"from"`       // who answered: "agent" (POST /v1/query, /v1/ops) or "resolve" (controld's POST /v1/resolve)
	Code       string       `json:"code"`       // the wire code; "transport" = no answer at all
	Status     int          `json:"status"`     // HTTP status; 0 for no answer
	RetryAfter bool         `json:"retryAfter"` // the answer carries Retry-After (the client waits at least that long)
	Action     ClientAction `json:"action"`
	Budget     string       `json:"budget"` // BudgetNone, BudgetEvery or BudgetAfterFirst
	// MayHaveApplied: a mutation that ends with this answer may have committed (the caller is told so and can
	// retry with the same request id).
	MayHaveApplied bool   `json:"mayHaveApplied"`
	Meaning        string `json:"meaning"`
}

// ContractPolicy are the numbers both clients share by default.
type ContractPolicy struct {
	MaxAttempts     int     `json:"maxAttempts"`     // attempts of one request on one route, the first included
	MaxReroutes     int     `json:"maxReroutes"`     // reroutes of one call
	BackoffBaseMS   int     `json:"backoffBaseMs"`   // first backoff ceiling; doubles per attempt, full jitter
	BackoffMaxMS    int     `json:"backoffMaxMs"`    // cap on a jittered backoff (a server hint may exceed it)
	MaxRetryAfterMS int     `json:"maxRetryAfterMs"` // longest Retry-After honored (MaxRetryAfter)
	BudgetRatio     float64 `json:"budgetRatio"`     // retry tokens earned per first attempt
	BudgetBurst     int     `json:"budgetBurst"`     // most tokens banked
}

// DefaultContractPolicy is what internal/client's and the SDK's defaults must equal (their vector tests check).
var DefaultContractPolicy = ContractPolicy{MaxAttempts: 3, MaxReroutes: 5, BackoffBaseMS: 50, BackoffMaxMS: 5000,
	MaxRetryAfterMS: int(MaxRetryAfter.Milliseconds()), BudgetRatio: 0.1, BudgetBurst: 10}

// resolveContract is controld's POST /v1/resolve (cmd/controld/api.go classifies its errors to these codes and
// statuses; TestResolveAnswersMatchTheContract there holds it to this table).
var resolveContract = []ContractRow{
	{"resolve", "ok", 200, false, ActDone, BudgetNone, false, "a route and a ticket"},
	{"resolve", "moving", 503, true, ActReroute, BudgetNone, false, "the database is moving or being created: resolve again after Retry-After (controld paces the client, so this reroute is not charged)"},
	{"resolve", "shed", 503, true, ActRetry, BudgetEvery, false, "controld refused the load (source rate limit, store reads or creates in flight); nothing ran"},
	{"resolve", "unavailable", 503, true, ActRetry, BudgetEvery, false, "Postgres or an agent cannot do it now, or too many calls in flight"},
	{"resolve", "no_capacity", 503, true, ActRetry, BudgetEvery, false, "no live vm under the placement caps right now"},
	{"resolve", "timeout", 504, false, ActRetry, BudgetEvery, false, "the resolve deadline passed"},
	{"resolve", "transport", 0, false, ActRetry, BudgetEvery, false, "no answer from controld"},
	{"resolve", "bad_request", 400, false, ActFail, BudgetNone, false, "malformed request"},
	{"resolve", "unauthorized", 401, false, ActFail, BudgetNone, false, "unknown, malformed or revoked credential"},
	{"resolve", "forbidden", 403, false, ActFail, BudgetNone, false, "the credential belongs to another tenant"},
	{"resolve", "not_found", 404, false, ActFail, BudgetNone, false, "no such database"},
	{"resolve", "suspended", 409, false, ActFail, BudgetNone, false, "the database is suspended"},
	{"resolve", "conflict", 409, false, ActFail, BudgetNone, false, "not allowed in the database's or tenant's state"},
	{"resolve", "quota_exceeded", 429, false, ActFail, BudgetNone, false, "create on first use past a cap: a quota, not an overload, so retrying cannot help"},
	{"resolve", "internal", 500, false, ActFail, BudgetNone, false, "a bug; report the log_id"},
}

// Contract is the whole caller contract: the agent's answers from Spec (in Codes order), an attempt that got
// no answer, then controld's resolve answers.
func Contract() []ContractRow {
	rows := make([]ContractRow, 0, len(Codes)+1+len(resolveContract))
	for _, c := range Codes {
		w := Spec[c]
		row := ContractRow{From: "agent", Code: string(c), Status: w.Status, RetryAfter: w.RetryAfter > 0, Action: w.Action, Meaning: w.Retry,
			MayHaveApplied: c == Expired || c == Internal}
		switch w.Action {
		case ActRetry:
			row.Budget = BudgetEvery
		case ActReroute:
			row.Budget = BudgetAfterFirst
		}
		rows = append(rows, row)
	}
	rows = append(rows, ContractRow{"agent", "transport", 0, false, ActReroute, BudgetAfterFirst, true,
		"no answer (connect failure, reset, unreadable body): the VM may be gone; a write may have committed, so it is retried with the same request id"})
	return append(rows, resolveContract...)
}

package det_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// simulated lists the packages a deterministic simulation drives (IR protocol plan §6.4). Each decides state
// from time, randomness or generated ids, so it must take them from det's Clock, Rand and IDGen. Paths are
// relative to internal/; subpackages are listed on their own. Adapters (objstore, placement/pg,
// placement/file, control/agenthttp, logx, metrics, stats, fsx, faultnet), the benchmark harnesses (bench,
// coldload, load, workload, schema: already seeded), errs (its timestamp is for logs only), cmd/ and every
// *test package are exempt.
var simulated = map[string]string{
	"actor":           "request expiry, group-commit batches, idle passivation, request-id retention",
	"breaker":         "closed/open/half-open transitions over a time window",
	"client":          "retry and reroute backoff with jitter, route refresh, call deadlines, request ids",
	"control":         "create/move/restore/resume decisions and their per-key deadlines",
	"control/locator": "route and API-key caches by TTL, ticket expiry, resolve deadlines",
	"doccompile":      "pure (IR-1): SQL and decoded rows are a function of schema, plan and rows only",
	"docexec":         "IR execution (IR-2b): _creationTime from Tx.Now, server ids from an injected IDGen (UUIDv7 from a det.Clock and det.Rand)",
	"docschema":       "pure (IR-1): validation and fingerprints are a function of their inputs only",
	"durable":         "manifest stamps, generation numbering, GC grace, the storage breaker",
	"edge":            "ticket gate, proxy deadlines, per-agent breakers, registry refresh cadence",
	"front":           "admission, per-request deadlines, readiness age",
	"ir":              "pure (IR-1): request decode and canonical JSON are a function of the bytes only",
	"keyfetch":        "when a rotated ticket key is fetched and installed (the verifier side of rotation)",
	"owner":           "fencing, reconcile rounds and their backoff",
	"ratelimit":       "token buckets refilled by elapsed time",
	"retry":           "backoff, jitter and the windowed retry budget",
	"ticket":          "expiry, clock skew, signing-key age",
	"vactor":          "activation deadlines and failure backoff, passivation, write-back cadence, GC",
}

// allowed are the justified direct uses, as "pkg/file.go: pkg.Name" → why.
var allowed = map[string]string{
	"front/http.go: time.Now": "net.Conn read/write deadlines are wall-clock instants (the HTTP adapter half of front)",
}

// sensitive reports whether an import path is one the rules cover (so it may not be dot- or blank-imported,
// which would hide its uses from the selector check).
func sensitive(path string) bool {
	switch path {
	case "time", "context", "math/rand", "math/rand/v2", "crypto/rand":
		return true
	}
	return isUUID(path)
}

// isUUID matches any uuid package, versioned or not (github.com/google/uuid, github.com/gofrs/uuid/v5).
func isUUID(path string) bool { return pkgBase(path) == "uuid" }

// pkgBase is the import path's last element, skipping a major-version suffix (/v2, /v5).
func pkgBase(path string) string {
	base := filepath.Base(path)
	if len(base) > 1 && base[0] == 'v' && strings.Trim(base[1:], "0123456789") == "" {
		base = filepath.Base(filepath.Dir(path))
	}
	return base
}

// banned reports whether a use of name from the package imported as path is banned, and why.
func banned(path, name string) (string, bool) {
	switch {
	case path == "time":
		switch name {
		case "Now", "Since", "Until", "After", "AfterFunc", "NewTimer", "NewTicker", "Tick", "Sleep":
			return "wall clock: use det.Clock", true
		}
	case path == "context":
		switch name {
		case "WithTimeout", "WithDeadline", "WithTimeoutCause", "WithDeadlineCause":
			return "wall-clock deadline: use det.WithTimeout / det.WithDeadline / Clock.WithDeadlineCause", true
		}
	case path == "math/rand" || path == "math/rand/v2":
		switch name {
		case "New", "NewPCG", "NewChaCha8", "NewZipf", "NewSource", "Rand", "Source", "PCG", "ChaCha8", "Zipf":
			return "", false // explicit sources and types are fine; only the package-level generator is banned
		}
		return "global random generator: use det.Rand", true
	case path == "crypto/rand":
		return "entropy: use det.Rand or det.IDGen", true
	case isUUID(path):
		return "generated ids: use det.IDGen", true
	}
	return "", false
}

// TestSimulatedPackagesUseSeams fails when a simulated package reads the wall clock, the global random
// generator, crypto entropy or a uuid package directly (DST-0). Test files are not checked.
func TestSimulatedPackagesUseSeams(t *testing.T) {
	root := filepath.Join("..") // internal/
	used := map[string]bool{}
	var bad []string
	for pkg := range simulated {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Errorf("simulated package %s: no Go files in %s (renamed? update the list)", pkg, dir)
			continue
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			for _, u := range bannedUses(t, file) {
				where := pkg + "/" + filepath.Base(file)
				key := where + ": " + u.sel
				if _, ok := allowed[key]; ok {
					used[key] = true
					continue
				}
				bad = append(bad, where+":"+strconv.Itoa(u.line)+": "+u.sel+" ("+u.why+")")
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
	for key := range allowed {
		if !used[key] {
			t.Errorf("allowlist entry %q matches nothing: remove it", key)
		}
	}
}

type use struct {
	sel, why string
	line     int
}

func bannedUses(t *testing.T, file string) []use {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	imports := map[string]string{} // local name → import path
	var out []use
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		name := pkgBase(path)
		if im.Name != nil {
			name = im.Name.Name
		}
		if (name == "." || name == "_") && sensitive(path) {
			out = append(out, use{sel: name + ` "` + path + `"`, why: "dot or blank import hides uses from this check",
				line: fset.Position(im.Pos()).Line})
			continue
		}
		imports[name] = path
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, ok := imports[x.Name]
		if !ok {
			return true
		}
		if why, ok := banned(path, sel.Sel.Name); ok {
			out = append(out, use{sel: x.Name + "." + sel.Sel.Name, why: why, line: fset.Position(sel.Pos()).Line})
		}
		return true
	})
	return out
}

// TestLintCatchesBannedUses checks the checker on a file that breaks every rule (mutation check).
func TestLintCatchesBannedUses(t *testing.T) {
	src := `package p

import (
	"context"
	crand "crypto/rand"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	uuid5 "github.com/gofrs/uuid/v5"
	. "time"
	_ "crypto/rand"
)

func f(ctx context.Context) {
	_ = time.Now()
	_ = time.Since
	<-time.After(time.Second)
	_, _ = context.WithTimeout(ctx, time.Second)
	_ = rand.IntN(3)
	_ = rand.New(rand.NewPCG(1, 2)) // allowed: explicit source
	_, _ = crand.Read(nil)
	_ = uuid.New()
	_ = uuid5.Must(uuid5.NewV7())
	_ = Now()
	var d time.Duration = time.Second // allowed: not a clock read
	_ = d
}
`
	file := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range bannedUses(t, file) {
		got = append(got, u.sel)
	}
	want := []string{`. "time"`, `_ "crypto/rand"`, "time.Now", "time.Since", "time.After", "context.WithTimeout", "rand.IntN", "crand.Read", "uuid.New", "uuid5.Must", "uuid5.NewV7"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("found %v, want %v", got, want)
	}
}

// Command frontbench measures the network front: the same point reads and inserts sent three ways, so the cost
// of each layer shows up as a difference.
//
//	frontbench keygen -out DIR                  issuer key (DIR/issuer.key) and agent key set (DIR/keys.json)
//	frontbench seed   -dir DIR -dbs N           N databases db-0..db-N-1 with sqlbench's generated schemas
//	frontbench run    -mode inproc|pipeline|http ...
//	frontbench locator -locator URL ...          through controld's locator with internal/client (locator.go)
//	frontbench verify -ack-logs F,... -dbs N     check a soak's acknowledged writes against the rows (soak.go)
//
// Modes: inproc calls the actor directly (Submit/Wait, as actor.Do does, plus row collection); pipeline runs
// front.Server.Handle in this process (ticket verify, admission, JSON decode, actor) without HTTP; http sends
// requests to a running agentd (-url, -proto h1|h2c). Schemas and statements come from internal/schema and
// internal/workload (read only), so a run is reproducible from -seed.
//
// Exit codes: 0 done, 1 failed, 2 bad usage, 70 bug, 130 interrupted.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "frontbench"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = "usage: frontbench keygen|seed|run|locator|verify [flags] (frontbench <cmd> -h for flags)"

func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if p := panicx.From(recover()); p != nil {
			fmt.Fprintf(stderr, "%v\n%s\nerror: something unexpected went wrong. This is a bug; please report the details above\n", p, p.Stack)
			code = exitBug
		}
	}()
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stderr)
	case "seed":
		err = seedCmd(args[1:], stderr)
	case "run":
		err = runCmd(ctx, args[1:], stdout, stderr)
	case "locator":
		err = locatorCmd(ctx, args[1:], stdout, stderr)
	case "verify":
		err = verifyCmd(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ue):
		return exitUsage
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		fmt.Fprintln(stderr, "interrupted")
		return exitInterrupted
	}
	if e, ok := errs.Outermost(err); ok && (e.Module == module || e.Module == "actor" || e.Module == "client") {
		fmt.Fprintf(stderr, "error: %s\n", e.Error())
		return exitError
	}
	fmt.Fprintf(stderr, "%v\nerror: something unexpected went wrong. This is a bug; please report the details above\n", err)
	return exitBug
}

type usageError struct{ error }

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "frontbench %s: unexpected arguments %q\n", fs.Name(), fs.Args())
		return usageError{errors.New("unexpected arguments")}
	}
	return nil
}

func keygen(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "output directory (required)")
	kid := fs.String("kid", "k1", "key id")
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	if *out == "" {
		fmt.Fprintln(stderr, "frontbench keygen: -out is required")
		return usageError{errors.New("missing -out")}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errs.Wrap(module, err, "cannot generate a key")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return errs.Wrap(module, err, "cannot create %s", *out)
	}
	set, _ := json.MarshalIndent(ticket.PublicKeyFile{Keys: []ticket.PublicKeyEntry{{KID: *kid, PublicKey: ticket.EncodeKey(pub)}}}, "", "  ")
	issuer, _ := json.Marshal(issuerFile{KID: *kid, Seed: ticket.EncodeKey(priv.Seed())})
	if err := os.WriteFile(filepath.Join(*out, "issuer.key"), issuer, 0o600); err != nil {
		return errs.Wrap(module, err, "cannot write issuer.key")
	}
	if err := os.WriteFile(filepath.Join(*out, "keys.json"), set, 0o644); err != nil {
		return errs.Wrap(module, err, "cannot write keys.json")
	}
	fmt.Fprintf(stderr, "wrote %s/issuer.key (private, mode 0600) and %s/keys.json (agent key set, kid %s)\n", *out, *out, *kid)
	return nil
}

// issuerFile is the private key file keygen writes and run reads.
type issuerFile struct {
	KID  string `json:"kid"`
	Seed string `json:"seed"` // base64url Ed25519 seed
}

func loadIssuer(path string) (string, ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, errs.Wrap(module, err, "cannot read issuer key").With("path", path)
	}
	var f issuerFile
	if err := json.Unmarshal(b, &f); err != nil {
		return "", nil, errs.Wrap(module, err, "invalid issuer key file %s", path)
	}
	priv, err := ticket.ParsePrivateSeed(f.Seed)
	if err != nil {
		return "", nil, errs.Wrap(module, err, "invalid issuer key file %s", path)
	}
	return f.KID, priv, nil
}

func dbID(i int) string { return fmt.Sprintf("db-%d", i) }

func seedCmd(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	dir := fs.String("dir", "", "database directory (required)")
	n := fs.Int("dbs", 16, "databases to create")
	seed := fs.Uint64("seed", 1, "schema and data seed")
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	if *dir == "" || *n < 1 {
		fmt.Fprintln(stderr, "frontbench seed: need -dir and -dbs >= 1")
		return usageError{errors.New("bad flags")}
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return errs.Wrap(module, err, "cannot create %s", *dir)
	}
	cat := schema.NewCatalog(*seed, schema.DefaultOptions())
	cat.Ensure(*n)
	for i := range *n {
		s := cat.Get(i)
		err := actor.Seed(filepath.Join(*dir, dbID(i)+".db"), actor.Config{}, s.DDL(),
			func(exec func(string, ...any) error) error { return s.Fill(*seed, exec) })
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(stderr, "seeded %d databases in %s (seed %d)\n", *n, *dir, *seed)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

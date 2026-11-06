package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
)

// import-mansio copies the Deno platform's tenancy (tenants, API keys, Better Auth users, passwords, unexpired
// sessions, memberships) into controld's Postgres schema (replace-platform plan §4.3; IR-4a, ADR 0012):
//
//	controld import-mansio -secrets FILE [-schema S] [-from-schema public] [-from-dsn-env NAME] [-dry-run]
//
// The target is the secrets file's pg_dsn (search_path -schema, as controld's pg.schema), migrated first. The
// source is the DSN in the environment variable -from-dsn-env (default: the target's), tables in -from-schema.
// One transaction; -dry-run rolls it back and prints what a real run would add. Rows already imported are
// skipped and one-way changes carried (deactivations, revocations, changed passwords; never a reactivation or a
// deletion: import_mansio.sql), so a re-run is safe. The source is opened read-only and never changed.
func importMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("controld import-mansio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	secrets := fs.String("secrets", "", "controld secrets file (pg_dsn: the target)")
	schema := fs.String("schema", "", "the target's search_path (controld's pg.schema)")
	fromSchema := fs.String("from-schema", "public", "the schema of the Deno platform's tables")
	fromEnv := fs.String("from-dsn-env", "", "environment variable holding the source DSN (default: the target DSN)")
	dry := fs.Bool("dry-run", false, "roll back: print what would be imported, change nothing")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *secrets == "" {
		fmt.Fprintln(stderr, "usage: controld import-mansio -secrets FILE [-schema S] [-from-schema public] [-from-dsn-env NAME] [-dry-run]")
		return exitUsage
	}
	sec, err := loadSecrets(*secrets)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	from := sec.PGDSN
	if *fromEnv != "" {
		if from = os.Getenv(*fromEnv); from == "" {
			fmt.Fprintf(stderr, "error: %s is empty: it must hold the source DSN\n", *fromEnv)
			return exitUsage
		}
	}
	p, err := pg.Open(pg.Config{DSN: sec.PGDSN, AppName: "controld/import-mansio", Schema: *schema, MaxConns: 2, Breaker: breaker.Config{}})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	defer p.Close()
	if _, err := p.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	src, err := pg.ReadMansio(ctx, from, *fromSchema)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	c, err := p.ImportMansio(ctx, src, *dry)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	verb := "imported"
	if *dry {
		verb = "would import (dry run, nothing changed)"
	}
	fmt.Fprintf(stdout, "%s: %d tenants, %d api keys, %d users, %d passwords, %d sessions, %d memberships; "+
		"carried: %d tenants deactivated, %d keys revoked, %d passwords changed\n", verb,
		c.Tenants, c.APIKeys, c.Users, c.Passwords, c.Sessions, c.Memberships, c.TenantsDeactivated, c.KeysRevoked, c.PasswordsChanged)
	if !src.HasAuth {
		fmt.Fprintln(stdout, "the source has no Better Auth tables: users, passwords and sessions were not looked for")
	}
	return exitOK
}

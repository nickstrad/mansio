package main

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// keygenMain is `controld keygen -dir DIR [-public FILE]`: it seeds an empty ticket key ring (ticket_keys.dir)
// with one signing key, in the format controld serves from (keyDir: <kid>.json, 0600, in a 0700 directory), and
// optionally writes the ring's public key set (ticket.PublicKeyFile) to FILE, which is what a static agentd or
// edge reads as keys_file. A fresh ring signs with its only key at once (ticket.PlanRotation's bootstrap), so
// controld started on the seeded directory issues tickets the -public file verifies.
//
// It refuses a directory that already holds keys: rotation is controld's job, and overwriting a live ring would
// orphan every ticket it signed. Exit codes as realMain's: 0 ok, 1 failed, 2 usage.
func keygenMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("controld keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "ticket key ring directory (controld's ticket_keys.dir; created 0700; must hold no keys)")
	public := fs.String("public", "", "also write the public key set here (an agentd/edge keys_file)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *dir == "" {
		fmt.Fprintln(stderr, "usage: controld keygen -dir DIR [-public FILE]")
		return exitUsage
	}
	ring := &keyDir{dir: *dir}
	have, _, err := ring.Load() // creates the directory 0700 and refuses unsafe modes
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	if len(have) > 0 {
		fmt.Fprintf(stderr, "error: %s already holds %d ticket key(s); controld rotates a live ring itself (remove the directory to start over)\n", *dir, len(have))
		return exitError
	}
	age, priv, err := ring.Create(time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	if *public != "" {
		set := ticket.NewPublicKeyFile(map[string]ed25519.PublicKey{age.KID: priv.Public().(ed25519.PublicKey)})
		b, _ := json.MarshalIndent(set, "", "  ")
		if err := fsx.WriteAtomic(*public, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: cannot write -public %s: %v\n", *public, err)
			return exitError
		}
	}
	fmt.Fprintf(stdout, "%s\n", age.KID)
	return exitOK
}

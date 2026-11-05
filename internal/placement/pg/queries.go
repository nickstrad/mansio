package pg

import (
	"embed"
	"fmt"
)

// One named query per file, loaded once (SQL G-1, G-2). The text is sent as is: values are always bind
// parameters (SQL G-4).
//
//go:embed queries/*.sql
var queryFS embed.FS

type queries struct {
	get, create, cas, moving, vmLoad, registerVM, invariants string
}

// q is read-only after init.
var q = queries{
	get:        mustQuery("get"),
	create:     mustQuery("create"),
	cas:        mustQuery("cas"),
	moving:     mustQuery("moving"),
	vmLoad:     mustQuery("vm_load"),
	registerVM: mustQuery("register_vm"),
	invariants: mustQuery("invariants"),
}

func mustQuery(name string) string {
	b, err := queryFS.ReadFile("queries/" + name + ".sql")
	if err != nil {
		panic(fmt.Sprintf("placement: embedded query %s missing: %v", name, err)) // build bug
	}
	return string(b)
}

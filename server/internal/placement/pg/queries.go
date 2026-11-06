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
	get, create, cas, moving, vmLoad, liveVMs, registerVM, invariants string
	// Registries (0002, CP-4).
	heartbeat, attestDead, vm, ownedBy, createDatabase, getDatabase, setDatabaseState, pending, dropPlacement string
	// Locator and API keys (0003, 0004, CP-5).
	locate, apiKey, createAPIKey, revokeAPIKey string
	// The app schema registry (0006, IR-2).
	registerSchema, getSchema, appRegistered string
	// Tenancy and auth (0007, IR-4a).
	tenancyCreateTenant, tenancyListTenants, tenancyGetTenant, tenancyDeactivateTenant, tenancyCreateKey, tenancyRevokeKey,
	tenancyKeyByID, tenancyKeyByLegacyHash, tenancyCreateUser, tenancyPassword, tenancyCreateSession, tenancySessionTenant,
	tenancyLink string
	// The Deno platform's tenancy import (IR-4a): source reads and the one target write.
	importHasAuth, importCore, importAuth, importMansio string
}

// q is read-only after init.
var q = queries{
	get:        mustQuery("get"),
	create:     mustQuery("create"),
	cas:        mustQuery("cas"),
	moving:     mustQuery("moving"),
	vmLoad:     mustQuery("vm_load"),
	liveVMs:    mustQuery("live_vms"),
	registerVM: mustQuery("register_vm"),
	invariants: mustQuery("invariants"),

	heartbeat:        mustQuery("heartbeat"),
	attestDead:       mustQuery("attest_dead"),
	vm:               mustQuery("vm"),
	ownedBy:          mustQuery("owned_by"),
	createDatabase:   mustQuery("create_database"),
	getDatabase:      mustQuery("get_database"),
	setDatabaseState: mustQuery("set_database_state"),
	pending:          mustQuery("pending"),
	dropPlacement:    mustQuery("drop_placement"),

	locate:       mustQuery("locate"),
	apiKey:       mustQuery("api_key"),
	createAPIKey: mustQuery("create_api_key"),
	revokeAPIKey: mustQuery("revoke_api_key"),

	registerSchema: mustQuery("register_schema"),
	getSchema:      mustQuery("get_schema"),
	appRegistered:  mustQuery("app_registered"),

	tenancyCreateTenant:     mustQuery("tenancy_create_tenant"),
	tenancyListTenants:      mustQuery("tenancy_list_tenants"),
	tenancyGetTenant:        mustQuery("tenancy_get_tenant"),
	tenancyDeactivateTenant: mustQuery("tenancy_deactivate_tenant"),
	tenancyCreateKey:        mustQuery("tenancy_create_key"),
	tenancyRevokeKey:        mustQuery("tenancy_revoke_key"),
	tenancyKeyByID:          mustQuery("tenancy_key_by_id"),
	tenancyKeyByLegacyHash:  mustQuery("tenancy_key_by_legacy_hash"),
	tenancyCreateUser:       mustQuery("tenancy_create_user"),
	tenancyPassword:         mustQuery("tenancy_password"),
	tenancyCreateSession:    mustQuery("tenancy_create_session"),
	tenancySessionTenant:    mustQuery("tenancy_session_tenant"),
	tenancyLink:             mustQuery("tenancy_link"),

	importHasAuth: mustQuery("import_mansio_has_auth"),
	importCore:    mustQuery("import_mansio_core"),
	importAuth:    mustQuery("import_mansio_auth"),
	importMansio:  mustQuery("import_mansio"),
}

func mustQuery(name string) string {
	b, err := queryFS.ReadFile("queries/" + name + ".sql")
	if err != nil {
		panic(fmt.Sprintf("placement: embedded query %s missing: %v", name, err)) // build bug
	}
	return string(b)
}

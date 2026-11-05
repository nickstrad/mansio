package edge

// Resolver maps a VM id (a ticket's vm claim) to its agent's query base URL (scheme://host:port). The edge asks
// it on every request, so it must answer from memory and never block.
//
// The seam for controld's VM registry: StaticRoutes (config) is the only implementation now; a resolver fed by
// the registry (polled or pushed, CP-5 and later) replaces it without touching the proxy. When an answer changes
// for a VM, the edge starts a fresh breaker and bulkhead for the new address.
type Resolver interface {
	Agent(vm string) (baseURL string, ok bool)
}

// StaticRoutes is a Resolver from configuration: VM id → base URL.
type StaticRoutes map[string]string

// Agent returns vm's base URL.
func (s StaticRoutes) Agent(vm string) (string, bool) {
	u, ok := s[vm]
	return u, ok
}

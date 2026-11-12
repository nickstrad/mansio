# Architecture decisions

| ADR                                                         | Decision                                                                           |
| ----------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| [0001](0001-effect-port.md)                                 | Effect port and separate native writer/reader process boundaries                   |
| [0002](0002-error-taxonomy.md)                              | Tagged error taxonomy and HTTP status mapping                                      |
| [0003](0003-k6-perf.md)                                     | k6 performance scenarios and thresholds                                            |
| [0004](0004-eventlog-duckdb-in-reader-only.md)              | Event-log DuckDB owned only by the reader process                                  |
| [0005](0005-text-storage-contract.md)                       | Text storage contract, including rejection of U+0000                               |
| [0006](0006-event-identity.md)                              | Event identity and event-log schema v2                                             |
| [0007](0007-ingest-before-retention.md)                     | Committed ingest before retention                                                  |
| [0008](0008-statement-execution-via-ffi.md)                 | Statement execution through the FFI binding over system libsqlite3                 |
| [0009](0009-intermediate-representation-is-the-app-wire.md) | The intermediate representation (IR) is the only app wire; handlers run in the SDK |
| [0010](0010-compiler-port-with-ts-oracle.md)                | Compiler ported to Go; the TS compiler is the oracle (vectors)                     |
| [0011](0011-database-identity-and-client-ids.md)            | `(tenant, app)` identifies a database; client ids are UUIDv7                       |
| [0012](0012-single-node-auth-port.md)                       | Single node: agentd checks credentials; multi-node: controld                       |
| [0013](0013-go-server-replaces-deno-platform.md)            | The Go server replaces the Deno platform (IR-6)                                    |

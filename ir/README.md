# ir: the intermediate representation protocol

IR stands for **intermediate representation**. It is the small, versioned protocol between the TypeScript SDK
and the Go server: instead of SQL, the SDK sends document operations (`select`, `get`, `insert`, `patch`,
`delete`, `assert`) as JSON to `POST /v1/ops`, and the server validates them, compiles them to SQLite SQL and
runs them. Application handlers run in the SDK; the server only ever sees these operations.

This folder is the protocol's definition, in no programming language, so both sides can be held to it:

| Path                  | What it is                                                                                                                                                                                                                                |
| --------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `spec/v1.md`          | The specification: the request envelope, the operations, preconditions, execution order, request ids and duplicates, responses and errors.                                                                                                |
| `spec/v1.schema.json` | A JSON Schema for the request envelope.                                                                                                                                                                                                   |
| `vectors/`            | Shared test cases (requests with their expected results). The Go server runs all of them and the TypeScript side runs the ones for the code it has, which is what keeps the two in agreement. See [vectors/README.md](vectors/README.md). |

Where the protocol is implemented:

- Go server: `server/internal/ir` (wire model and strict decoding), `server/internal/docexec` (execution) and
  `server/internal/doccompile` (operations to SQL).
- TypeScript: `core/ir/` (types and rules) and `sdk/` (the client that produces requests).

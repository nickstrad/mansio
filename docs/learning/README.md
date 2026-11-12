# Learning paths

Three ordered paths through mansio, one per kind of reader. Each is a numbered list of things to run, read or
try, with one to three sentences on why each step matters. Work a path top to bottom.

| Path                         | For                                                                | You will touch                                                                     |
| ---------------------------- | ------------------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| [customer.md](customer.md)   | Someone building an application on the platform                    | `examples/tic-tac-toe/`, the SDK's public API, `docs/guide.md`                     |
| [operator.md](operator.md)   | Someone running the platform                                       | configs, `controld`/`agentctl`/`dbtool`, metrics                                   |
| [developer.md](developer.md) | Someone who wants to understand the platform in order to extend it | the SDK internals, the intermediate representation (IR) spec, the Go server, tests |

Suggested order if you want all three: customer, then developer, then operator. The customer path explains
what the platform promises, the developer path how it keeps the promise, and the operator path how to run it
and what to do when it cannot.

All three use the same practice setup: `make platform-up` (the whole platform on loopback; Linux only) or one
agentd started by hand from `docs/guide.md`, and the diagrams at <https://nickstrad.github.io/mansio/>.

[typescript-tour.md](typescript-tour.md) is a short tour of the TypeScript side with excerpts from the source;
the developer path reads it at step 6. Its excerpts are copied by hand, so check them against the source if
they look off.

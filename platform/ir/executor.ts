/**
 * The IR reference executor (`ir/spec/v1.md` §6–§7): runs a decoded request against one document
 * database file, atomically, with today's compiler, today's DDL and today's `_platform_schema`.
 *
 * Why here and not in `core/`: it owns a SQLite connection, a transaction and the request-id table.
 * The rules it applies are `core/`'s — `core/compile/*` for SQL, `core/ir/semantics.ts` for `$ref`,
 * bodies and `expect` — so this file is I/O and sequencing only. It is the oracle the shared vectors
 * run against in TS, and the shim IR-3 puts behind `/v1/ops` on the Deno backend.
 *
 * Determinism: the only inputs besides the database and the request are Effect's `Clock` (read once
 * per request: every `_creationTime` and the request-id row's `applied_ms` share that reading) and
 * `IdGen` (drawn once per insert without a client id, in op order). Nothing else is random or timed.
 *
 * Statements run on the binding's blocking path and emit no events: the reference executor has no
 * deadline, queue or event sink. Integrating those is `Storage`'s job when IR-3 wires this in.
 */
import { Clock, Effect, type Scope } from "effect";
import { NotFoundError, SchemaDriftError, type SchemaError, StorageError } from "../../core/errors.ts";
import type { CompileError } from "../../core/errors.ts";
import { type Schema, storable } from "../../core/schema.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect, tableFor } from "../../core/compile/select.ts";
import { compileDelete, compileInsert, compileReplace } from "../../core/compile/write.ts";
import { decode } from "../../core/compile/decode.ts";
import type { Row, SQL } from "../../core/compile/dialect.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { FingerprintMismatch, type IrCause, IrFailure, PreconditionConflict } from "../../core/ir/errors.ts";
import { canonical } from "../../core/ir/json.ts";
import { fingerprintText } from "../../core/ir/schema.ts";
import { body, checkExpect, type Doc, resolve, resolveId } from "../../core/ir/semantics.ts";
import { type Op, READ_OPS, type Request, type Response } from "../../core/ir/types.ts";
import { IdGen } from "../services.ts";
import { Database, prepareCached } from "../sqlite_ffi/mod.ts";

/** mansio-server's request-id table (`internal/actor/reqid.go`), byte for byte. */
export const REQUEST_TABLE_DDL = [
  `create table if not exists _mansio_requests (
  request_id text    not null primary key,
  applied_ms integer not null,
  result     blob
) strict, without rowid`,
  `create index if not exists _mansio_requests_applied on _mansio_requests (applied_ms)`,
];

/** The version byte of an IR result in `_mansio_requests.result`: then the canonical JSON of `result`. */
export const OUTCOME_IR = 2;

export interface IrDatabase {
  /** `sha256:` + hex SHA-256 of the fingerprint text. */
  readonly fingerprint: string;
  readonly execute: (request: Request) => Effect.Effect<Response, IrFailure, IdGen>;
}

export interface IrDatabaseOptions {
  readonly file: string;
  readonly schema: Schema;
  readonly libraryPath?: string;
}

/** `sha256:` + lowercase hex of SHA-256 over the UTF-8 bytes of `text`. */
export function sha256Fingerprint(text: string): Effect.Effect<string> {
  return Effect.promise(async () => {
    const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
    return `sha256:${Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("")}`;
  });
}

/**
 * Opens (creating if needed) the database file for `schema`, inside the caller's scope: the same
 * fingerprint check and DDL as `platform/sqlite.ts`'s startup, plus the request-id table.
 */
export function openIrDatabase(
  options: IrDatabaseOptions,
): Effect.Effect<IrDatabase, StorageError | SchemaDriftError | CompileError | SchemaError, Scope.Scope> {
  const { file, schema } = options;
  return Effect.gen(function* () {
    // Every statement is fetched from the binding's cache and used in the same step (its contract);
    // `close` finalizes the cache.
    const db = yield* Effect.acquireRelease(
      Effect.try({
        try: () => Database.open(file, { libraryPath: options.libraryPath }),
        catch: (cause) => new StorageError({ operation: "open", detail: String(cause), cause }),
      }),
      (db) => Effect.sync(() => db.isOpen && db.close()),
    );

    const attempt = <A>(operation: StorageError["operation"], table: string | undefined, run: () => A) =>
      Effect.try({
        try: run,
        catch: (cause) => new StorageError({ operation, table, detail: String(cause), cause }),
      });
    const all = (sql: SQL, table?: string) =>
      attempt("select", table, () => prepareCached(db, sql.text).all(sql.values) as Row[]);
    const run = (operation: "insert" | "update" | "delete", sql: SQL, table?: string) =>
      attempt(operation, table, () => prepareCached(db, sql.text).run(sql.values).changes);
    const exec = (operation: "begin" | "commit" | "rollback" | "ddl" | "pragma", text: string) =>
      attempt(operation, undefined, () => db.exec(text));

    /** `use` inside `begin` … `COMMIT`, rolled back on every exit that did not commit. */
    const transaction = <A, E, R>(begin: string, use: Effect.Effect<A, E, R>) =>
      Effect.acquireUseRelease(
        exec("begin", begin),
        () => Effect.zipLeft(use, exec("commit", "COMMIT")),
        () => Effect.sync(() => db.inTransaction && db.exec("ROLLBACK")),
      );

    const text = fingerprintText(schema);
    const fingerprint = yield* sha256Fingerprint(text);
    yield* exec("pragma", "PRAGMA journal_mode=WAL");
    yield* exec("pragma", "PRAGMA synchronous=FULL");
    yield* exec("pragma", "PRAGMA busy_timeout=5000");
    yield* transaction(
      "BEGIN IMMEDIATE",
      Effect.gen(function* () {
        yield* exec(
          "ddl",
          'CREATE TABLE IF NOT EXISTS "_platform_schema" (id INTEGER PRIMARY KEY CHECK (id = 1), definition TEXT NOT NULL) STRICT',
        );
        const [existing] = yield* all({
          text: 'SELECT definition FROM "_platform_schema" WHERE id = 1',
          values: [],
        });
        if (existing && existing.definition !== text) {
          return yield* new SchemaDriftError({ file, detail: "stored schema fingerprint differs" });
        }
        for (const statement of yield* compileDDL(schema)) yield* exec("ddl", statement);
        for (const statement of REQUEST_TABLE_DDL) yield* exec("ddl", statement);
        if (!existing) {
          yield* run("insert", { text: 'INSERT INTO "_platform_schema" VALUES (1, ?)', values: [text] });
        }
      }),
    );

    /** The current document, or `null`. */
    const load = (table: string, id: string) =>
      Effect.gen(function* () {
        const plan: SelectPlan = {
          kind: "select",
          table,
          filters: [{ field: "_id", op: "eq", value: id }],
          limit: 1,
        };
        const rows = yield* all(yield* compileSelect(schema, plan, "sqlite"), table);
        return ((yield* decode(schema, plan, rows)) as Doc[])[0] ?? null;
      });

    /** One operation; `ids[i]` is insert `i`'s `_id`, `now` the request's clock reading. */
    const step = (op: Op, ids: (string | undefined)[], now: number): Effect.Effect<unknown, IrCause, IdGen> =>
      Effect.gen(function* () {
        const table = yield* tableFor(schema, op.table);
        switch (op.op) {
          case "select": {
            const plan: SelectPlan = {
              kind: "select",
              table: op.table,
              filters: op.filters.map((f) => ({ ...f, value: resolve(f.value, ids) })),
              limit: op.limit,
              ...(op.order ? { order: op.order } : {}),
              ...(op.aggregate ? { aggregate: op.aggregate } : {}),
            };
            const sql = yield* compileSelect(schema, plan, "sqlite");
            for (const filter of plan.filters) yield* storable(filter.value, filter.field);
            return yield* decode(schema, plan, yield* all(sql, op.table));
          }
          case "get":
            return yield* load(op.table, resolveId(op.id, ids));
          case "insert": {
            const value = yield* body(table, resolve(op.value, ids), false);
            if (op.id !== undefined && (yield* load(op.table, op.id)) !== null) {
              return yield* new PreconditionConflict({
                table: op.table,
                id: op.id,
                check: "exists",
                expected: false,
                actual: true,
              });
            }
            const id = op.id ?? (yield* (yield* IdGen).next);
            yield* run(
              "insert",
              yield* compileInsert(schema, op.table, value, { _id: id, _creationTime: now }),
              op.table,
            );
            return id;
          }
          case "patch":
          case "delete":
          case "assert": {
            const id = resolveId(op.id, ids);
            const patch = op.op === "patch" ? yield* body(table, resolve(op.value, ids), true) : undefined;
            const doc = yield* load(op.table, id);
            if (op.expect) {
              yield* checkExpect(table, op.table, id, resolve(op.expect, ids) as typeof op.expect, doc);
            }
            if (op.op === "assert") return null;
            if (op.op === "delete") {
              if (doc !== null) yield* run("delete", yield* compileDelete(schema, op.table, id), op.table);
              return { deleted: doc !== null };
            }
            if (doc === null) return yield* new NotFoundError({ table: op.table, id });
            const current = Object.fromEntries(Object.keys(table.fields).map((key) => [key, doc[key]]));
            yield* run(
              "update",
              yield* compileReplace(schema, op.table, id, { ...current, ...patch }),
              op.table,
            );
            return { _version: doc._version + 1 };
          }
        }
      });

    const ops = (request: Request, now: number) =>
      Effect.gen(function* () {
        const ids: (string | undefined)[] = [];
        const result: unknown[] = [];
        for (const [i, op] of request.ops.entries()) {
          const value = yield* Effect.mapError(
            step(op, ids, now),
            (cause) => new IrFailure({ cause, op: i }),
          );
          ids.push(op.op === "insert" ? value as string : undefined);
          result.push(value);
        }
        return result;
      });

    /** A mutation: answered from `_mansio_requests` when its id was applied, otherwise run and recorded. */
    const mutation = (request: Request, requestId: string, now: number) =>
      Effect.gen(function* () {
        const [prior] = yield* all({
          text: "select result from _mansio_requests where request_id = ?",
          values: [requestId],
        });
        if (prior) return { result: yield* decodeOutcome(prior.result), duplicate: true as const };
        const result = yield* ops(request, now);
        const outcome = new Uint8Array([OUTCOME_IR, ...new TextEncoder().encode(canonical(result))]);
        yield* attempt("insert", "_mansio_requests", () =>
          prepareCached(db, "insert into _mansio_requests (request_id, applied_ms, result) values (?, ?, ?)")
            .run([requestId, now, outcome]));
        return { result };
      });

    const execute = (request: Request): Effect.Effect<Response, IrFailure, IdGen> =>
      Effect.gen(function* () {
        if (request.schema !== fingerprint) {
          return yield* new FingerprintMismatch({ expected: fingerprint, actual: request.schema });
        }
        const now = yield* Clock.currentTimeMillis;
        // Decode guarantees this; a request built any other way is a caller bug, not a client error.
        if (request.kind === "query" && request.ops.some((op) => !READ_OPS.includes(op.op))) {
          return yield* Effect.dieMessage("IR executor: a query holds a non-read operation");
        }
        if (request.kind === "query") return { result: yield* transaction("BEGIN", ops(request, now)) };
        // Decode guarantees a mutation carries its request id.
        return yield* transaction("BEGIN IMMEDIATE", mutation(request, request.requestId!, now));
      }).pipe(Effect.mapError((e) => e instanceof IrFailure ? e : new IrFailure({ cause: e })));

    return { fingerprint, execute };
  });
}

/** The stored result of an applied request: version byte `OUTCOME_IR`, then canonical JSON. */
function decodeOutcome(stored: unknown): Effect.Effect<unknown[], StorageError> {
  const failed = new StorageError({
    operation: "select",
    table: "_mansio_requests",
    detail: "unreadable result",
  });
  if (!(stored instanceof Uint8Array) || stored[0] !== OUTCOME_IR) return Effect.fail(failed);
  return Effect.try({
    try: () => JSON.parse(new TextDecoder().decode(stored.subarray(1))) as unknown[],
    catch: () => failed,
  });
}

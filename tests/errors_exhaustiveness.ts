/**
 * Compile-only assertions over the error taxonomy. Nothing here runs: it is checked by
 * `deno task check` (which lists this file) in the same way `tests/types.ts` guards the typed SDK.
 *
 * What it proves:
 *
 * 1. `toHttpStatus` is **total** over `PlatformError`. The `default` branch in `core/errors.ts`
 *    narrows to `never`, so a new union member without a case is a type error. `statusByTag` below
 *    is a `Record` keyed by the tag union, so a new member without an entry is a type error here
 *    too — belt and braces, in two files, which is the point of a contract item.
 * 2. The tag inventory is **frozen**. `TagsAreExactly` fails if a tag is added to or removed from
 *    `PlatformError` without updating this list, which forces the change to be deliberate.
 * 3. `toHttpStatus` **rejects** a tagged error that is not in the union (the `@ts-expect-error`).
 * 4. The four union aliases the plan names (`DbError`, `MutationError`, `QueryError`,
 *    `PlatformError`) nest as documented, and the never-served unions stay out of `PlatformError`.
 *    FFI-05's `StatementDeadlineError` joins at `DbError`, not inside the IPC-facing unions.
 */
import { Data } from "effect";
import * as E from "../core/errors.ts";

// --- type-level assertion helpers ------------------------------------------

type Equals<A, B> = (<X>() => X extends A ? 1 : 2) extends (<X>() => X extends B ? 1 : 2) ? true
  : false;
type Expect<T extends true> = T;
/** `A` is assignable to `B`. Used to assert one union nests inside another. */
type Extends<A, B> = [A] extends [B] ? true : false;

// --- 1. toHttpStatus accepts every member and only members -----------------

const _acceptsThePlatformUnion: (error: E.PlatformError) => number = E.toHttpStatus;

/**
 * Independent restatement of §1.3. `Record<PlatformError["_tag"], number>` is exhaustive by
 * construction: omit a tag and this object literal fails to typecheck. The values are asserted
 * against `toHttpStatus` is key-set only — value parity with `toHttpStatus` is a runtime
 * concern for item 22's tests.
 */
const statusByTag: Record<E.PlatformError["_tag"], number> = {
  MalformedTenantId: 400,
  UnknownTenant: 404,
  TenantDeactivated: 403,
  SchemaError: 400,
  CompileError: 400,
  RuleError: 400,
  HandlerKindMismatch: 400,
  AuthError: 401,
  RouteNotFound: 404,
  UnknownAppError: 404,
  UnknownHandlerError: 404,
  NotFoundError: 404,
  UnknownAnalysisQuery: 404,
  ConflictError: 409,
  BackupAlreadyRunning: 409,
  RequestTooLarge: 413,
  DecodeError: 500,
  ContextExpiredError: 500,
  NonSerializableResultError: 500,
  StorageError: 500,
  ReaderError: 500,
  QueryWriteAttemptError: 500,
  InvalidKeyError: 500,
  BackupError: 500,
  SnapshotIntegrityError: 500,
  IngestError: 500,
  AnalysisError: 500,
  IpcError: 502,
  ReaderExitedError: 502,
  ObjectStoreError: 502,
  QueueFullError: 503,
  StatementDeadlineError: 504,
};

/** `statusByTag`'s keys are exactly the union's tags — no extras, none missing. */
type _StatusTableIsExact = Expect<Equals<keyof typeof statusByTag, E.PlatformError["_tag"]>>;

// --- 2. the frozen tag inventory -------------------------------------------

/**
 * Every tag that can reach an HTTP response, as of item 02. Changing `PlatformError` without
 * changing this list is a compile error, so the taxonomy cannot grow by accident.
 */
type FrozenPlatformTags =
  | "MalformedTenantId"
  | "UnknownTenant"
  | "TenantDeactivated"
  | "SchemaError"
  | "CompileError"
  | "DecodeError"
  | "RuleError"
  | "RouteNotFound"
  | "UnknownAppError"
  | "UnknownHandlerError"
  | "HandlerKindMismatch"
  | "AuthError"
  | "RequestTooLarge"
  | "NotFoundError"
  | "ConflictError"
  | "StorageError"
  | "ContextExpiredError"
  | "NonSerializableResultError"
  | "ReaderError"
  | "QueryWriteAttemptError"
  | "QueueFullError"
  | "IpcError"
  | "ReaderExitedError"
  | "ObjectStoreError"
  | "InvalidKeyError"
  | "BackupError"
  | "BackupAlreadyRunning"
  | "SnapshotIntegrityError"
  | "IngestError"
  | "AnalysisError"
  | "UnknownAnalysisQuery"
  // FFI-05: an unbounded statement stopped at its deadline.
  | "StatementDeadlineError";

type _TagsAreExactly = Expect<Equals<E.PlatformError["_tag"], FrozenPlatformTags>>;

/** The never-served tags, kept out of `PlatformError` so `toHttpStatus` can never be asked for one. */
type _NeverServedTags = Expect<
  Equals<
    E.StartupError["_tag"] | E.CliError["_tag"] | E.ClientError["_tag"],
    "ConfigError" | "SchemaDriftError" | "RestoreError" | "RemoteError" | "Unavailable"
  >
>;
type _StartupIsNotServed = Expect<Equals<Extends<E.StartupError, E.PlatformError>, false>>;
type _ClientIsNotServed = Expect<Equals<Extends<E.ClientError, E.PlatformError>, false>>;
type _CliIsNotServed = Expect<Equals<Extends<E.CliError, E.PlatformError>, false>>;
type _AnyErrorCoversEverything = Expect<
  Extends<E.PlatformError | E.StartupError | E.CliError | E.ClientError, E.AnyError>
>;

// --- 3. a foreign tag is rejected ------------------------------------------

class NotInTheTaxonomy extends Data.TaggedError("NotInTheTaxonomy")<{ readonly why: string }> {}

export function foreignTagsAreRejected() {
  // @ts-expect-error a tag outside PlatformError has no status and must not be accepted
  E.toHttpStatus(new NotInTheTaxonomy({ why: "not a member of the union" }));
}

// --- 4. the union aliases nest as documented -------------------------------

type _DbIsMutationPlusQueryPlusDeadline = Expect<
  Equals<E.DbError, E.MutationError | E.QueryError | E.StatementDeadlineError>
>;
type _MutationIsADbError = Expect<Extends<E.MutationError, E.DbError>>;
type _QueryIsADbError = Expect<Extends<E.QueryError, E.DbError>>;
type _DbIsServed = Expect<Extends<E.DbError, E.PlatformError>>;
type _RemoteReaderIsServed = Expect<Extends<E.RemoteReaderError, E.PlatformError>>;

/** Write-path-only tags must not leak into the read-path union, and vice versa. */
type _WriteOnlyStaysWriteOnly = Expect<
  Equals<Extends<E.NonSerializableResultError, E.QueryError>, false>
>;
type _ReadOnlyStaysReadOnly = Expect<
  Equals<Extends<E.QueryWriteAttemptError, E.MutationError>, false>
>;

/**
 * The deadline is raised only by the in-process SQLite paths, never by the DuckDB reader process, so
 * it stays out of the unions that cross the reader IPC (whose tags are listed exhaustively there).
 */
type _DeadlineIsNotRemote = Expect<Equals<Extends<E.StatementDeadlineError, E.RemoteReaderError>, false>>;
type _DeadlineIsNotInQueryError = Expect<Equals<Extends<E.StatementDeadlineError, E.QueryError>, false>>;
type _DeadlineIsNotInMutationError = Expect<
  Equals<Extends<E.StatementDeadlineError, E.MutationError>, false>
>;
type _DeadlineIsServed = Expect<Extends<E.StatementDeadlineError, E.PlatformError>>;

// --- the payload shapes the plan's own examples rely on --------------------

export function documentedConstructorsCompile() {
  // plan §1.4, verbatim shape
  const notFound = new E.NotFoundError({ table: "games", id: "id-1" });
  const conflict = new E.ConflictError({ expected: 1, actual: 2 });
  // item 02's own examples
  const schema = new E.SchemaError({
    path: "cell",
    expected: "integer",
    actual: 0.5,
    reason: "type-mismatch",
  });
  const compile = new E.CompileError({ reason: "unsupported-operator", field: "status" });
  const queueFull = new E.QueueFullError({ queue: "writer", depth: 1000, cap: 1000 });
  // FFI-05's payload
  const deadline = new E.StatementDeadlineError({
    deadlineMs: 5000,
    elapsedMs: 5000.4,
    table: "games",
    statementKind: "count",
  });
  return [notFound, conflict, schema, compile, queueFull, deadline].map(E.toHttpStatus);
}

/** Keeps the type-only assertions from being erased as unused by the compiler. */
export type Assertions = [
  _StatusTableIsExact,
  _TagsAreExactly,
  _NeverServedTags,
  _StartupIsNotServed,
  _ClientIsNotServed,
  _CliIsNotServed,
  _AnyErrorCoversEverything,
  _DbIsMutationPlusQueryPlusDeadline,
  _MutationIsADbError,
  _QueryIsADbError,
  _DbIsServed,
  _RemoteReaderIsServed,
  _WriteOnlyStaysWriteOnly,
  _ReadOnlyStaysReadOnly,
  _DeadlineIsNotRemote,
  _DeadlineIsNotInQueryError,
  _DeadlineIsNotInMutationError,
  _DeadlineIsServed,
];

export const statuses = statusByTag;
export const accepts = _acceptsThePlatformUnion;

/**
 * IR failures and their wire form (`intermediate-representation/spec/v1.md` §8).
 *
 * PURE: `effect` and `../errors.ts` only.
 *
 * Two IR-only failures reuse an existing wire tag on purpose, so a caller that already switches on
 * the tag needs no new case:
 *
 * - `PreconditionConflict` answers `ConflictError` (409). `core/errors.ts`'s `ConflictError` carries
 *   two numeric versions; a failed `expect` also reports `exists` and `fields` checks, whose expected
 *   and actual values are not numbers, so it gets its own payload under the same tag.
 * - `FingerprintMismatch` answers `SchemaDriftError` (409). The core class names a file, which a
 *   network caller must not see; this one names the two fingerprints.
 *
 * `IrFailure` wraps whichever failure ended a request with the index of the operation it belongs to,
 * when there is one.
 */
import { Data } from "effect";
import {
  type CompileError,
  type DecodeError,
  type NotFoundError,
  type PlatformError,
  type RequestTooLarge,
  type SchemaError,
  type StatementDeadlineError,
  type StorageError,
  toHttpStatus,
} from "../errors.ts";

/**
 * Which `expect` member failed. `exists` also covers an insert whose client id is already taken;
 * `unchanged` is a table assert (no `id`): `expected` is its `unchangedSince`, `actual` the sequence of
 * the table's last change (below `expected` when the database has not reached that sequence, spec §5.5).
 */
export type PreconditionCheck = "exists" | "version" | "fields" | "unchanged";

export class PreconditionConflict extends Data.TaggedError("ConflictError")<{
  readonly table: string;
  readonly id?: string;
  readonly check: PreconditionCheck;
  readonly expected: unknown;
  readonly actual: unknown;
  /** The commit sequence of the state the request ran against (spec §8.1). */
  readonly seq?: number;
  /** The state behind every precondition of the request that does not hold in that state. */
  readonly current?: readonly CurrentState[];
  /** `current` was cut short at the size cap: a precondition missing from it may fail too. */
  readonly currentTruncated?: true;
}> {}

/**
 * One entry of a conflict's `current` (spec §8.1): the document behind operation `op`'s precondition
 * (`doc`, `null` when there is none), or for a table precondition the sequence of the table's last change.
 */
export type CurrentState =
  | {
    readonly op: number;
    readonly table: string;
    readonly id: string;
    readonly doc: Record<string, unknown> | null;
  }
  | { readonly op: number; readonly table: string; readonly changed: number };

export class FingerprintMismatch extends Data.TaggedError("SchemaDriftError")<{
  /** The database's fingerprint. */
  readonly expected: string;
  /** The request's `schema`. */
  readonly actual: string;
}> {}

/** Everything that can end an IR request. */
export type IrCause =
  | SchemaError
  | CompileError
  | DecodeError
  | NotFoundError
  | PreconditionConflict
  | FingerprintMismatch
  | RequestTooLarge
  | StorageError
  | StatementDeadlineError;

export class IrFailure extends Data.TaggedError("IrFailure")<{
  readonly cause: IrCause;
  /** The index of the operation the failure belongs to; absent for envelope-level failures. */
  readonly op?: number;
}> {}

/** The `{ error }` body of a failed request, and its status. */
export type WireError = {
  status: number;
  body: {
    error: { tag: string; message: string; op?: number; fields?: Record<string, unknown> };
  };
};

/** The HTTP status of a cause: `core/errors.ts`'s mapping, plus the two IR-only classes. */
export function statusOf(cause: IrCause): number {
  if (cause instanceof PreconditionConflict || cause instanceof FingerprintMismatch) return 409;
  return toHttpStatus(cause as PlatformError);
}

/** The structured payload of a cause: its own fields minus the tag and any driver `cause`. */
function payload(cause: IrCause): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(cause)) {
    if (key !== "_tag" && key !== "cause" && value !== undefined) out[key] = value;
  }
  return out;
}

/**
 * The wire form of a failure. 4xx bodies carry `fields`, the cause's structured payload; 5xx bodies
 * carry only the tag, because their detail is about the server (it goes to the server's log).
 */
export function toWireError(failure: IrFailure): WireError {
  const { cause, op } = failure;
  const status = statusOf(cause);
  const fields = payload(cause);
  const message = status >= 500
    ? cause._tag
    : `${cause._tag}: ${Object.entries(fields).map(([k, v]) => `${k}=${JSON.stringify(v)}`).join(" ")}`;
  return {
    status,
    body: {
      error: {
        tag: cause._tag,
        message,
        ...(op === undefined ? {} : { op }),
        ...(status >= 500 ? {} : { fields }),
      },
    },
  };
}

/**
 * The intermediate representation (IR) protocol v1 data model (`ir/spec/v1.md`): what a decoded `/v1/ops` request is.
 *
 * PURE: types and constants only. The wire form and this form are the same shape; `decode.ts` is
 * what turns untrusted JSON into one of these, and `json.ts`'s `canonical` turns one back into text.
 */
import type { Filter } from "../plan.ts";

/** Most operations one request may carry. */
export const MAX_OPS = 64;
/** The body cap, in bytes of UTF-8. The same 64 KiB as the HTTP surface's default. */
export const MAX_BODY_BYTES = 65_536;
/** Longest `requestId`, in bytes (mansio-server `front.MaxRequestID`). */
export const MAX_ID_BYTES = 64;

/** A back-reference to the `_id` produced by insert number `$ref` of the same request. */
export type Ref = { $ref: number };
/** An id position: a literal id, or a reference to an earlier insert's id. */
export type IdRef = string | Ref;

/**
 * Preconditions on one document. All present members must hold. `version` and `fields` imply
 * `exists: true`; `exists: false` stands alone.
 */
export type Expect = {
  version?: number;
  exists?: boolean;
  fields?: Record<string, unknown>;
};

export type SelectOp = {
  op: "select";
  table: string;
  filters: Filter[];
  order?: { field: string; direction: "asc" | "desc" };
  limit: number;
  aggregate?: { kind: "count" } | { kind: "countBy"; field: string };
};
export type GetOp = { op: "get"; table: string; id: IdRef };
export type InsertOp = { op: "insert"; table: string; value: Record<string, unknown>; id?: string };
export type PatchOp = {
  op: "patch";
  table: string;
  id: IdRef;
  value: Record<string, unknown>;
  expect?: Expect;
};
export type DeleteOp = { op: "delete"; table: string; id: IdRef; expect?: Expect };
export type AssertOp = { op: "assert"; table: string; id: IdRef; expect: Expect };
/**
 * An assert without an id: the database has reached the commit sequence `unchangedSince`, and no applied
 * mutation has changed a row of `table` since (spec §5.5). It is how a select is guarded: whatever the select answered at that
 * sequence, it still answers.
 */
export type TableAssertOp = { op: "assert"; table: string; expect: { unchangedSince: number } };

export type Op = SelectOp | GetOp | InsertOp | PatchOp | DeleteOp | AssertOp | TableAssertOp;
export type OpName = Op["op"];

/** The operations a `query` may contain: the reads, and `assert` (a precondition on the same snapshot). */
export const QUERY_OPS: readonly OpName[] = ["select", "get", "assert"];

export type Kind = "query" | "mutation";

export type Request = {
  v: 1;
  db: { tenant: string; app: string };
  kind: Kind;
  /** Required for a mutation; optional and ignored for a query. */
  requestId?: string;
  /** `sha256:` + hex of the schema fingerprint text the client compiled against. */
  schema: string;
  ops: Op[];
};

/**
 * A successful response body. `seq` is the database's commit sequence (spec §6.5): of the snapshot a query
 * read, or after a mutation. `duplicate` appears only when it is `true`, and then there is no `seq`.
 */
export type Response = { result: unknown[]; seq?: number; duplicate?: true };

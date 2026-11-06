/**
 * The IR protocol v1 data model (`ir/spec/v1.md`): what a decoded `/v1/ops` request is.
 *
 * PURE: types and constants only. The wire form and this form are the same shape; `decode.ts` is
 * what turns untrusted JSON into one of these, and `json.ts`'s `canonical` turns one back into text.
 */
import type { Filter } from "../plan.ts";

/** Protocol version carried in every envelope's `v`. */
export const VERSION = 1;
/** Most operations one request may carry. */
export const MAX_OPS = 64;
/** The body cap, in bytes of UTF-8. The same 64 KiB as the HTTP surface's default (`platform/config.ts`). */
export const MAX_BODY_BYTES = 65_536;
/** Longest `requestId`, `db.tenant` or `db.app`, in bytes (ts-db-go `front.MaxRequestID`). */
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

export type Op = SelectOp | GetOp | InsertOp | PatchOp | DeleteOp | AssertOp;
export type OpName = Op["op"];

/** The operations a `query` may contain. */
export const READ_OPS: readonly OpName[] = ["select", "get"];

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

/** A successful response body. `duplicate` appears only when it is `true`. */
export type Response = { result: unknown[]; duplicate?: true };

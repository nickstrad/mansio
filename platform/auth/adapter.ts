/**
 * Better Auth 1.7.7's narrow email/password adapter: handwritten PostgreSQL,
 * postgres.js only. No ORM/query builder. Values always use bind parameters;
 * identifiers are checked against the pinned core schema. Unknown operations fail closed.
 */
import {
  type AdapterFactory,
  createAdapterFactory,
  type CustomAdapter,
} from "npm:better-auth@1.7.7/adapters";
import { Data } from "effect";
import type { BetterAuthOptions } from "npm:better-auth@1.7.7";
import type { Sql, TransactionSql } from "npm:postgres@3.4.9";

/** Internal adapter contract failure; HTTP boundaries redact its detail. */
export class AuthAdapterError extends Data.TaggedError("AuthAdapterError")<{ readonly detail: string }> {}

const fields: Record<string, readonly string[]> = {
  user: ["id", "name", "email", "emailVerified", "image", "createdAt", "updatedAt"],
  session: ["id", "expiresAt", "token", "createdAt", "updatedAt", "ipAddress", "userAgent", "userId"],
  account: [
    "id",
    "accountId",
    "providerId",
    "userId",
    "accessToken",
    "refreshToken",
    "idToken",
    "accessTokenExpiresAt",
    "refreshTokenExpiresAt",
    "scope",
    "password",
    "createdAt",
    "updatedAt",
  ],
  verification: ["id", "identifier", "value", "expiresAt", "createdAt", "updatedAt"],
};
type Conditions = Parameters<CustomAdapter["findOne"]>[0]["where"];
type Client = Sql | TransactionSql;
function table(model: string): string {
  if (!Object.hasOwn(fields, model)) throw new AuthAdapterError({ detail: "Unsupported auth model" });
  return '"' + model + '"';
}
function column(model: string, name: string): string {
  table(model);
  if (!fields[model].includes(name)) throw new AuthAdapterError({ detail: "Unsupported auth field" });
  return '"' + name + '"';
}

/** Bind all values; explicitly reject unsupported operators rather than weaken a predicate. */
function statement(model: string) {
  const values: unknown[] = [];
  const bind = (value: unknown) => {
    values.push(value);
    return "$" + values.length;
  };
  const where = (conditions: Conditions = []) => {
    if (conditions.length === 0) return "TRUE";
    const clauses = conditions.map((condition) => {
      let field = column(model, condition.field);
      let value = condition.value;
      if (condition.mode === "insensitive") {
        if (typeof value !== "string") {
          throw new AuthAdapterError({ detail: "Unsupported insensitive predicate" });
        }
        field = "lower(" + field + ")";
        value = value.toLowerCase();
      }
      const op = condition.operator;
      if (value === null) {
        if (op !== "eq" && op !== "ne") throw new AuthAdapterError({ detail: "Unsupported null predicate" });
        return field + (op === "eq" ? " IS NULL" : " IS NOT NULL");
      }
      if (op === "in" || op === "not_in") {
        if (!Array.isArray(value)) throw new AuthAdapterError({ detail: "Invalid set predicate" });
        if (!value.length) return op === "in" ? "FALSE" : "TRUE";
        return field + (op === "in" ? " IN (" : " NOT IN (") + value.map(bind).join(",") + ")";
      }
      const operators = { eq: "=", ne: "<>", lt: "<", lte: "<=", gt: ">", gte: ">=" };
      if (!Object.hasOwn(operators, op)) throw new AuthAdapterError({ detail: "Unsupported auth predicate" });
      return field + " " + operators[op as keyof typeof operators] + " " + bind(value);
    });
    // Match the pinned adapter: AND predicates intersect the grouped OR predicates.
    const and = clauses.filter((_, i) => conditions[i].connector !== "OR");
    const or = clauses.filter((_, i) => conditions[i].connector === "OR");
    return [and.length ? "(" + and.join(" AND ") + ")" : "", or.length ? "(" + or.join(" OR ") + ")" : ""]
      .filter(Boolean).join(" AND ");
  };
  const assignments = (data: Record<string, unknown>) => {
    const entries = Object.entries(data).filter(([, value]) => value !== undefined);
    if (!entries.length) throw new AuthAdapterError({ detail: "Empty auth mutation" });
    return entries.map(([key, value]) => column(model, key) + "=" + bind(value)).join(",");
  };
  return { values, bind, where, assignments };
}

export function postgresAdapter(sql: Client, inTransaction = false): AdapterFactory<BetterAuthOptions> {
  return (options: BetterAuthOptions) =>
    createAdapterFactory({
      config: {
        adapterId: "mansio-postgres",
        supportsDates: true,
        supportsBooleans: true,
        supportsNumericIds: false,
        transaction: inTransaction
          ? false
          : async (run) =>
            await (sql as Sql).begin(async (tx) => await run(postgresAdapter(tx, true)(options))) as Awaited<
              ReturnType<typeof run>
            >,
      },
      adapter: () => {
        const query = async (text: string, values: unknown[]) =>
          await sql.unsafe(text, values as Parameters<Sql["unsafe"]>[1]);
        const remove = async (model: string, where: Conditions, one: boolean) => {
          const s = statement(model);
          const target = table(model);
          const predicate = s.where(where);
          return await query(
            "DELETE FROM " + target + " WHERE " +
              (one
                ? predicate + ' AND "id" IN (SELECT "id" FROM ' + target + " WHERE " + predicate + " LIMIT 1)"
                : predicate) +
              " RETURNING *",
            s.values,
          );
        };
        return {
          async create({ model, data }) {
            const s = statement(model);
            const entries = Object.entries(data).filter(([, value]) => value !== undefined);
            const rows = await query(
              "INSERT INTO " + table(model) + " (" + entries.map(([key]) => column(model, key)).join(",") +
                ") VALUES (" + entries.map(([, value]) => s.bind(value)).join(",") + ") RETURNING *",
              s.values,
            );
            return rows[0] as unknown as typeof data;
          },
          async findOne<T>({ model, where }: Parameters<CustomAdapter["findOne"]>[0]) {
            const s = statement(model);
            const rows = await query(
              "SELECT * FROM " + table(model) + " WHERE " + s.where(where) + " LIMIT 1",
              s.values,
            );
            return (rows[0] ?? null) as T | null;
          },
          async findMany<T>(
            { model, where, sortBy, limit, offset }: Parameters<CustomAdapter["findMany"]>[0],
          ) {
            const s = statement(model);
            const order = sortBy
              ? " ORDER BY " + column(model, sortBy.field) + (sortBy.direction === "desc" ? " DESC" : " ASC")
              : "";
            return await query(
              "SELECT * FROM " + table(model) + " WHERE " + s.where(where) + order +
                " LIMIT " + s.bind(limit) + " OFFSET " + s.bind(offset ?? 0),
              s.values,
            ) as unknown as T[];
          },
          async update<T>({ model, where, update }: Parameters<CustomAdapter["update"]>[0]) {
            const s = statement(model);
            const set = s.assignments(update as Record<string, unknown>);
            const target = table(model);
            const predicate = s.where(where);
            const rows = await query(
              "UPDATE " + target + " SET " + set + " WHERE " + predicate +
                ' AND "id" IN (SELECT "id" FROM ' + target +
                " WHERE " + predicate + " LIMIT 1) RETURNING *",
              s.values,
            );
            return (rows[0] ?? null) as T | null;
          },
          async updateMany({ model, where, update }) {
            const s = statement(model);
            return (await query(
              "UPDATE " + table(model) + " SET " + s.assignments(update) +
                " WHERE " + s.where(where) + " RETURNING id",
              s.values,
            )).length;
          },
          async delete({ model, where }) {
            await remove(model, where, true);
          },
          async deleteMany({ model, where }) {
            return (await remove(model, where, false)).length;
          },
          async consumeOne<T>({ model, where }: Parameters<NonNullable<CustomAdapter["consumeOne"]>>[0]) {
            return ((await remove(model, where, true))[0] ?? null) as T | null;
          },
          async count({ model, where }) {
            const s = statement(model);
            const rows = await query(
              "SELECT count(*) AS n FROM " + table(model) + " WHERE " + s.where(where),
              s.values,
            );
            return Number(rows[0].n);
          },
        } satisfies CustomAdapter;
      },
    })(options);
}

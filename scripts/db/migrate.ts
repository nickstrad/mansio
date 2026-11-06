/**
 * `deno task db:migrate` / `make db-migrate` — hand-rolled Postgres migration runner (plan MT-01b).
 *
 * No migration framework, no ORM (standing rule): plain SQL files, hand-written, discovered from
 * `platform/tenants/migrations/` as `<NNN>_<slug>.up.sql` / `<NNN>_<slug>.down.sql` pairs ordered
 * by `<NNN>`.
 *
 * Usage:
 *   deno task db:migrate        apply all pending migrations in order (no-op when the journal is current)
 *   deno task db:migrate down   roll back the most recently applied migration via its `.down.sql`
 *
 * How it works:
 * - `DATABASE_URL` is read from MT-02's config options table (`platform/config.ts`); this script
 *   adds no env handling of its own.
 * - The journal table `schema_migrations(version text PRIMARY KEY, applied_at timestamptz)` is
 *   created by the runner when absent.
 * - Before planning, the runner takes `pg_advisory_xact_lock` so concurrent runners serialize.
 *   The lock is transaction-scoped, so it is held inside a single outer transaction spanning
 *   planning + apply; each migration then applies inside its own SAVEPOINT (a subtransaction)
 *   together with its journal row — the same per-migration atomicity as a standalone
 *   transaction, while the lock stays held for the whole run. A failed migration rolls back to
 *   its savepoint (leaving earlier migrations committed) and the runner exits non-zero.
 * - Migrations must be transaction-safe SQL: no `CONCURRENTLY`, no `VACUUM`.
 * - Down-migration safety: before running a `.down.sql`, every `DROP TABLE` target is checked
 *   for rows; a down migration that would drop a non-empty table fails loudly instead of
 *   silently discarding data.
 *
 * Better Auth convention (MT-08): the Better Auth tables are generated ONCE with Better Auth's
 * own tooling and captured verbatim as a numbered `<NNN>_<slug>` migration pair. They are never
 * regenerated in place afterwards — a re-capture is a new migration pair.
 *
 * Exit codes: 0 success / no-op; 1 operational failure; 2 usage error.
 */
import postgres, { type Sql } from "npm:postgres@3.4.9";
import { Effect } from "effect";
import { ConfigLive } from "../../platform/config.ts";
import { Config } from "../../platform/services.ts";

const MIGRATIONS_DIR = new URL("../../platform/tenants/migrations/", import.meta.url);
const FILE_RE = /^(\d{3})_([A-Za-z0-9][A-Za-z0-9_.-]*)\.(up|down)\.sql$/;
const ADVISORY_LOCK_SQL = "SELECT pg_advisory_xact_lock(hashtext('mansio-migrate'))";
const JOURNAL_DDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);`;
const USAGE = `usage:
  deno task db:migrate        apply pending migrations (no-op when current)
  deno task db:migrate down   roll back the most recently applied migration`;

class UsageError extends Error {
  override readonly name = "UsageError";
}

interface Migration {
  readonly version: string;
  readonly slug: string;
  readonly upPath: URL;
  readonly downPath: URL;
}

interface DiscoveredFile {
  readonly slug: string;
  readonly path: URL;
}

/** Discover and validate `<NNN>_<slug>.up.sql` / `.down.sql` pairs, ordered by NNN. */
async function discoverMigrations(): Promise<Migration[]> {
  const ups = new Map<string, DiscoveredFile>();
  const downs = new Map<string, DiscoveredFile>();
  for await (const entry of Deno.readDir(MIGRATIONS_DIR)) {
    if (!entry.isFile) continue;
    const match = FILE_RE.exec(entry.name);
    if (!match) {
      // Hidden files (e.g. .DS_Store) are ignored; anything else is likely a misnamed
      // migration, and a silently-skipped migration is worse than a loud failure.
      if (entry.name.startsWith(".")) continue;
      throw new Error(
        `unrecognized file in migrations dir: ${entry.name} ` +
          `(expected <NNN>_<slug>.up.sql / .down.sql); refusing to plan`,
      );
    }
    const [, version, slug, direction] = match;
    const path = new URL(entry.name, MIGRATIONS_DIR);
    const slot = direction === "up" ? ups : downs;
    if (slot.has(version)) throw new Error(`duplicate migration version ${version}: ${entry.name}`);
    slot.set(version, { slug, path });
  }
  const migrations: Migration[] = [];
  for (const [version, up] of [...ups].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
    const down = downs.get(version);
    if (!down) {
      throw new Error(
        `migration ${version}_${up.slug} has no .down.sql; every migration needs a rollback pair`,
      );
    }
    if (down.slug !== up.slug) {
      throw new Error(`migration ${version}: up slug "${up.slug}" != down slug "${down.slug}"`);
    }
    migrations.push({ version, slug: up.slug, upPath: up.path, downPath: down.path });
  }
  for (const version of downs.keys()) {
    if (!ups.has(version)) throw new Error(`orphan .down.sql for version ${version} with no .up.sql`);
  }
  return migrations;
}

/** DATABASE_URL from MT-02's config options table — no env handling of our own. */
async function readDatabaseUrl(): Promise<string> {
  return await Effect.runPromise(
    Effect.provide(
      Effect.map(Config, (config) => config.tenancy.databaseUrl),
      ConfigLive,
    ),
  );
}

/** Host/db for operator output — never the password. */
function safeTarget(databaseUrl: string): string {
  try {
    const u = new URL(databaseUrl);
    return `${u.hostname}:${u.port === "" ? "5432" : u.port}${u.pathname}`;
  } catch {
    return "<unparseable DATABASE_URL>";
  }
}

async function migrateUp(sql: Sql, migrations: Migration[]): Promise<void> {
  const outcome = await sql.begin(async (tx) => {
    await tx.unsafe(ADVISORY_LOCK_SQL);
    await tx.unsafe(JOURNAL_DDL);
    // Re-read the journal inside the lock: another runner may have applied while we waited.
    const journalRows = await tx`SELECT version FROM schema_migrations`;
    const applied = new Set(journalRows.map((row) => row.version as string));
    const known = new Set(migrations.map((m) => m.version));
    for (const version of applied) {
      if (!known.has(version)) {
        throw new Error(
          `journal lists applied migration ${version} but no files exist for it; refusing to plan`,
        );
      }
    }
    const maxApplied = [...applied].sort().pop();
    const pending = migrations.filter((m) => !applied.has(m.version));
    for (const m of pending) {
      if (maxApplied !== undefined && m.version < maxApplied) {
        throw new Error(
          `migration ${m.version}_${m.slug} is older than already-applied ${maxApplied}; ` +
            `refusing to apply out of order`,
        );
      }
    }
    let appliedCount = 0;
    let failure: string | null = null;
    for (const m of pending) {
      const savepoint = `mig_${m.version}`;
      const upSql = await Deno.readTextFile(m.upPath);
      await tx.unsafe(`SAVEPOINT "${savepoint}"`);
      try {
        await tx.unsafe(upSql);
        await tx`INSERT INTO schema_migrations (version) VALUES (${m.version})`;
        await tx.unsafe(`RELEASE SAVEPOINT "${savepoint}"`);
      } catch (err) {
        await tx.unsafe(`ROLLBACK TO SAVEPOINT "${savepoint}"`);
        failure = `${m.version}_${m.slug}: ${err instanceof Error ? err.message : String(err)}`;
        break;
      }
      appliedCount++;
      console.log(`applied ${m.version}_${m.slug}`);
    }
    return { appliedCount, failure };
  });
  if (outcome.failure !== null) throw new Error(`migration failed: ${outcome.failure}`);
  if (outcome.appliedCount === 0) console.log("schema_migrations is current; nothing to do");
}

/** Strip `--` line comments and `/* ... *\/` blocks so DROP TABLE parsing sees only real SQL. */
function stripSqlComments(sqlText: string): string {
  return sqlText.replace(/--[^\n]*/g, "").replace(/\/\*[\s\S]*?\*\//g, "");
}

/**
 * Bare table names targeted by DROP TABLE in a down migration. Anything unparseable fails
 * loudly — the down-safety check must never silently skip a target it cannot verify.
 */
function dropTableTargets(downSql: string): string[] {
  const targets: string[] = [];
  const re = /\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([^;]+)/gi;
  let m: RegExpExecArray | null;
  const body = stripSqlComments(downSql);
  while ((m = re.exec(body)) !== null) {
    const list = m[1].replace(/\b(?:CASCADE|RESTRICT)\b/gi, " ");
    for (const raw of list.split(",")) {
      const bare = raw.trim().replace(/^"+|"+$/g, "").split(".").pop()!.trim().replace(
        /^"+|"+$/g,
        "",
      );
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(bare)) {
        throw new Error(
          `cannot verify down-safety: unparseable DROP TABLE target ${JSON.stringify(raw.trim())}; ` +
            `refusing to run`,
        );
      }
      targets.push(bare);
    }
  }
  return [...new Set(targets)];
}

async function migrateDown(sql: Sql, migrations: Migration[]): Promise<void> {
  await sql.begin(async (tx) => {
    await tx.unsafe(ADVISORY_LOCK_SQL);
    await tx.unsafe(JOURNAL_DDL);
    const latest = await tx`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`;
    if (latest.length === 0) {
      throw new Error("schema_migrations journal is empty: nothing to roll back");
    }
    const version = latest[0]!.version as string;
    const m = migrations.find((mm) => mm.version === version);
    if (!m) {
      throw new Error(`journaled migration ${version} has no files on disk; refusing to roll back`);
    }
    const downSql = await Deno.readTextFile(m.downPath);
    // Down-safety: a down migration that would drop a non-empty table fails loudly.
    for (const table of dropTableTargets(downSql)) {
      const countRows = await tx`SELECT count(*)::int AS n FROM ${tx(table)}`;
      const n = countRows[0]!.n as number;
      if (n > 0) {
        throw new Error(
          `refusing to roll back ${version}_${m.slug}: DROP TABLE "${table}" would discard ` +
            `${n} live row(s)`,
        );
      }
    }
    await tx.unsafe(downSql);
    await tx`DELETE FROM schema_migrations WHERE version = ${version}`;
    console.log(`rolled back ${version}_${m.slug}`);
  });
}

async function main(): Promise<void> {
  const [direction, ...rest] = Deno.args;
  if (rest.length > 0 || (direction !== undefined && direction !== "down")) throw new UsageError(USAGE);
  const databaseUrl = await readDatabaseUrl();
  console.log(`migrate: target ${safeTarget(databaseUrl)}`);
  // Notices (e.g. `relation "schema_migrations" already exists, skipping` from the IF NOT EXISTS
  // journal DDL) are routine runner chatter, not operator signal: silence them. Errors still throw.
  const sql = postgres(databaseUrl, { max: 1, onnotice: () => {} });
  try {
    const migrations = await discoverMigrations();
    if (direction === "down") await migrateDown(sql, migrations);
    else await migrateUp(sql, migrations);
  } finally {
    await sql.end();
  }
}

try {
  await main();
} catch (err) {
  if (err instanceof UsageError) {
    console.error(err.message);
    Deno.exit(2);
  }
  console.error(`migrate: ${err instanceof Error ? err.message : String(err)}`);
  Deno.exit(1);
}

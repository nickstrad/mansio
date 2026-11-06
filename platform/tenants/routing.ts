/**
 * MT-04 routing seam. A future ControlPlaneRouter implements TenantStorageRouter and is selected
 * by tenantRouter: "control-plane" plus endpoint configuration as a different layer. Storage,
 * request middleware and clients keep the same service contract.
 *
 * Locations are capabilities issued by resolve, not user-supplied paths. The SQLite capability
 * opens synchronously after rechecking containment and symlinks, including SQLite sidecars.
 * The configured directory must be owned by the server (not writable by untrusted local users):
 * SQLite opens by path (`sqlite3_open_v2` takes no file descriptor), so a prevalidated fd cannot be
 * handed to it, and no path API can defeat a hostile local rename race.
 *
 * Connections are the FFI binding's (`platform/sqlite_ffi/`), on the library `Config.sqlite.library`
 * names, the same one `Storage` and the document reader use: one process never opens a file through
 * two SQLite copies.
 */
import { Context, Effect, Either, Layer } from "effect";
import { closeSync, constants, lstatSync, mkdirSync, openSync, realpathSync } from "node:fs";
import { dirname, join, resolve as absolute } from "node:path";
import { Database } from "../sqlite_ffi/mod.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import type { Schema } from "../../core/schema.ts";
import { StorageError, type TenantError, UnknownTenant } from "../../core/errors.ts";
import { Config } from "../services.ts";
import { parseTenantId, type TenantId } from "./registry.ts";

export { parseTenantId, type TenantId } from "./registry.ts";

const locationBrand: unique symbol = Symbol("StorageLocation");
export type StorageLocation = {
  readonly [locationBrand]: true;
  readonly _tag: "SqliteFile";
  readonly path: string;
  /**
   * Driver capability: a read-write FFI connection, opened in the same synchronous step as the
   * location's re-check. The caller owns it and closes it.
   */
  readonly open: () => Database;
};

export interface TenantStorageRouterService {
  readonly resolve: (tenantId: TenantId) => Effect.Effect<StorageLocation, TenantError>;
  readonly prepare: (location: StorageLocation) => Effect.Effect<void, TenantError>;
}

export class TenantStorageRouter extends Context.Tag("mansio/TenantStorageRouter")<
  TenantStorageRouter,
  TenantStorageRouterService
>() {}

/** Inject with Effect.provideService per request. Never store the current tenant in a global Ref. */
export class TenantContext extends Context.Tag("mansio/TenantContext")<
  TenantContext,
  { readonly tenantId: TenantId }
>() {}

/**
 * No I/O until prepare/open; resolve only validates and constructs a deterministic location.
 * `libraryPath` is `Config.sqlite.library` (absent: the binding's default list).
 */
export function makeLocalFolderRouter(
  tenantsDir: string,
  schema: Schema,
  libraryPath?: string,
): TenantStorageRouterService {
  const root = absolute(tenantsDir);
  const issued = new WeakSet<StorageLocation>();
  const locations = new Map<TenantId, StorageLocation>();
  const ddl = compileDDL(schema);
  let canonicalRoot: string | undefined;

  const check = (path: string) => {
    mkdirSync(root, { recursive: true, mode: 0o700 });
    if (lstatSync(root).isSymbolicLink()) {
      throw new StorageError({ operation: "open", detail: "Tenant directory is a symlink" });
    }
    const currentRoot = realpathSync(root);
    if (canonicalRoot !== undefined && canonicalRoot !== currentRoot) {
      throw new StorageError({ operation: "open", detail: "Tenant directory changed" });
    }
    canonicalRoot = currentRoot;
    for (const candidate of [path, `${path}-wal`, `${path}-shm`, `${path}-journal`]) {
      try {
        const stat = lstatSync(candidate);
        if (stat.isSymbolicLink() || !stat.isFile() || stat.nlink !== 1) {
          throw new StorageError({
            operation: "open",
            detail: "Tenant storage must be a regular, unlinked file",
          });
        }
        if (dirname(realpathSync(candidate)) !== currentRoot) {
          throw new StorageError({ operation: "open", detail: "Tenant path escaped" });
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      }
    }
    // Exclusive creation refuses a symlink even for a previously missing file.
    try {
      closeSync(openSync(path, constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600));
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    }
    if (lstatSync(path).isSymbolicLink() || dirname(realpathSync(path)) !== currentRoot) {
      throw new StorageError({ operation: "open", detail: "Tenant path escaped at open" });
    }
  };

  return {
    resolve: (input) =>
      Effect.map(parseTenantId(input), (tenantId) => {
        const existing = locations.get(tenantId);
        if (existing) return existing;
        const path = join(root, `${tenantId}.db`);
        const location: StorageLocation = Object.freeze({
          [locationBrand]: true as const,
          _tag: "SqliteFile" as const,
          path,
          open: () => {
            check(path);
            return Database.open(path, { libraryPath });
          },
        });
        issued.add(location);
        locations.set(tenantId, location);
        return location;
      }),
    prepare: (location) =>
      Effect.try({
        try: () => {
          if (!issued.has(location)) {
            throw new StorageError({ operation: "open", detail: "Location was not issued by this router" });
          }
          if (Either.isLeft(ddl)) {
            throw ddl.left;
          }
          const db = location.open();
          try {
            db.exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL");
            db.exec("BEGIN IMMEDIATE");
            db.exec(
              'CREATE TABLE IF NOT EXISTS "_platform_schema" (id INTEGER PRIMARY KEY CHECK (id = 1), definition TEXT NOT NULL) STRICT',
            );
            const fingerprint = JSON.stringify(schema);
            const existing = db.prepare('SELECT definition FROM "_platform_schema" WHERE id = 1').get();
            if (existing && existing.definition !== fingerprint) {
              throw new StorageError({ operation: "open", detail: "Schema/storage changed" });
            }
            for (const sql of ddl.right) db.exec(sql);
            if (!existing) db.prepare('INSERT INTO "_platform_schema" VALUES (1, ?)').run([fingerprint]);
            db.exec("COMMIT");
          } finally {
            // A short-lived connection on the blocking path: `close` finalizes the statements above.
            try {
              if (db.inTransaction) db.exec("ROLLBACK");
            } finally {
              db.close();
            }
          }
        },
        // TenantError deliberately stays outside PlatformError until the HTTP tenancy items land.
        catch: (cause) => Object.assign(new UnknownTenant({}), { cause }),
      }),
  };
}

/** Selected from Config, never from environment reads in Storage. */
export const LocalFolderRouter = (schema: Schema): Layer.Layer<TenantStorageRouter, TenantError, Config> =>
  Layer.effect(
    TenantStorageRouter,
    Effect.gen(function* () {
      const { tenancy, sqlite } = yield* Config;
      if (tenancy.router !== "local") return yield* new UnknownTenant({});
      return makeLocalFolderRouter(tenancy.tenantsDir, schema, sqlite.library);
    }),
  );

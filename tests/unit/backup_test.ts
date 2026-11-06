/**
 * Unit tests for the `Backups` service (plan item 19).
 *
 * Item 20's `StorageTest` / `ObjectStoreTest` had not landed when this was written, so the doubles are
 * local and inline (owned by this item, per the plan): a `Storage` whose `snapshot` writes a small real
 * SQLite file (or garbage, or blocks on a gate), and an in-memory `ObjectStore` with per-key failure and
 * gate injection. The event-ordering and round-trip tests use the real `StorageLive` and the real
 * `ObjectStoreLocal`, because the phase-versus-transfer accounting (item 35, ADR 0006) is about the real
 * store's events. The accounting test at the end goes further: it serializes a run's events, ingests
 * them through `makeEventLogIngest`, and counts backups, phases, transfers and retries in DuckDB.
 *
 * The three behaviours the plan calls out get the most attention: a failed snapshot-object upload leaves
 * `latest.json` unchanged; interrupting `take` mid-upload releases the per-app guard and removes the
 * temp directory; two concurrent `take`s for one app yield exactly one `BackupAlreadyRunning`.
 *
 * The real-S3 round trip at the end runs only with `S3_INTEGRATION=1` (see
 * `tests/integration/object_store_s3_test.ts` for the SeaweedFS setup).
 */
import { after, test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { AwsClient } from "aws4fetch";
import { Cause, Deferred, Effect, Exit, Fiber, Layer, Option } from "effect";
import {
  BackupAlreadyRunning,
  BackupError,
  ObjectStoreError,
  SnapshotIntegrityError,
  StorageError,
} from "../../core/errors.ts";
import { type BackupEventRecord, type EventRecord, toJsonLine } from "../../core/events.ts";
import {
  BackupsLive,
  integrityCheck,
  makeBackupRunner,
  type SnapshotSource,
  verifySnapshot,
} from "../../platform/backup.ts";
import { ConfigTest } from "../../platform/config.ts";
import { makeEventLogIngest } from "../../platform/eventlog/ingest.ts";
import { makeObjectStoreLocal, makeObjectStoreS3, sha256 } from "../../platform/object_store.ts";
import {
  Backups,
  EventSink,
  IdGen,
  type Manifest,
  ObjectStore,
  type ObjectStoreService,
  Storage,
} from "../../platform/services.ts";
import { StorageLive } from "../../platform/sqlite.ts";
import { schema } from "../../app/schema.ts";
import { functions } from "../../app/functions.ts";
import { inspect } from "../helpers/sqlite_inspect.ts";

const enc = new TextEncoder();
const dec = new TextDecoder();

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

/** Writes a tiny, valid, standalone SQLite database at `path`. */
function writeValidSnapshot(path: string, marker = "snapshot"): void {
  const db = inspect(path, { write: true });
  try {
    db.exec("CREATE TABLE t (v TEXT)");
    db.run("INSERT INTO t (v) VALUES (?)", [marker]);
  } finally {
    db.close();
  }
}

interface StubStorage extends SnapshotSource {
  /** Every path `snapshot` was asked to write; its directory is the run's temp directory. */
  readonly paths: string[];
}

type SnapshotMode =
  | { readonly kind: "valid" }
  | { readonly kind: "garbage" }
  | { readonly kind: "fail" }
  | { readonly kind: "die" }
  | {
    readonly kind: "gate";
    readonly entered: Deferred.Deferred<void>;
    readonly release: Deferred.Deferred<void>;
  };

function stubStorage(mode: SnapshotMode = { kind: "valid" }): StubStorage {
  const paths: string[] = [];
  return {
    paths,
    snapshot: (path) =>
      Effect.gen(function* () {
        paths.push(path);
        switch (mode.kind) {
          case "garbage":
            writeFileSync(path, "this is not a database, just bytes long enough to look like one".repeat(4));
            return;
          case "fail":
            return yield* new StorageError({ operation: "snapshot", detail: "disk full" });
          case "die":
            return yield* Effect.die(new Error("driver crashed"));
          case "gate":
            yield* Deferred.succeed(mode.entered, undefined);
            yield* Deferred.await(mode.release);
            writeValidSnapshot(path);
            return;
          default:
            writeValidSnapshot(path);
        }
      }),
  };
}

interface StubStore {
  readonly service: ObjectStoreService;
  readonly objects: Map<string, Uint8Array>;
  /** Keys in the order `put` was called, failed puts included. */
  readonly puts: string[];
  /** Fail every put whose key satisfies the predicate. */
  failWhen(predicate: (key: string) => boolean): void;
  /** Park every put whose key satisfies the predicate: `entered` fires, then it waits forever. */
  hangWhen(predicate: (key: string) => boolean, entered: Deferred.Deferred<void>): void;
  unhang(): void;
}

function stubStore(seed: Record<string, Uint8Array> = {}): StubStore {
  const objects = new Map(Object.entries(seed));
  const puts: string[] = [];
  let fail: (key: string) => boolean = () => false;
  let hang: { predicate: (key: string) => boolean; entered: Deferred.Deferred<void> } | undefined;
  return {
    objects,
    puts,
    failWhen: (predicate) => void (fail = predicate),
    hangWhen: (predicate, entered) => void (hang = { predicate, entered }),
    unhang: () => void (hang = undefined),
    service: {
      put: (key, bytes) =>
        Effect.gen(function* () {
          puts.push(key);
          if (fail(key)) {
            return yield* new ObjectStoreError({ operation: "put", key, status: 503, detail: "injected" });
          }
          if (hang?.predicate(key)) {
            yield* Deferred.succeed(hang.entered, undefined);
            return yield* Effect.never;
          }
          objects.set(key, bytes.slice());
        }),
      get: (key) =>
        Effect.suspend(() => {
          const bytes = objects.get(key);
          return bytes
            ? Effect.succeed(bytes.slice())
            : Effect.fail(new ObjectStoreError({ operation: "get", key, status: 404 }));
        }),
    },
  };
}

interface Run {
  readonly events: EventRecord[];
  readonly deps: Layer.Layer<EventSink | IdGen>;
}

function collector(): Run {
  const events: EventRecord[] = [];
  let id = 0;
  return {
    events,
    deps: Layer.mergeAll(
      Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
      Layer.succeed(IdGen, { next: Effect.sync(() => `id-${++id}`) }),
    ),
  };
}

/** A runner over the stub store, with events collected. */
function runnerOver(store: StubStore) {
  const run = collector();
  const runner = Effect.runSync(
    makeBackupRunner().pipe(
      Effect.provideService(ObjectStore, store.service),
      Effect.provide(run.deps),
    ),
  );
  return { runner, events: run.events };
}

const backupEvents = (events: EventRecord[]) =>
  events.filter((e): e is BackupEventRecord => e.kind === "backup");

const failureOf = <A, E>(exit: Exit.Exit<A, E>): unknown => {
  assert.ok(Exit.isFailure(exit), "expected a failure");
  const failure = Cause.failureOption(exit.cause);
  assert.ok(Option.isSome(failure), "expected a failure option");
  return failure.value;
};

const OLD_LATEST = enc.encode('{"format":1,"appId":"app","note":"previous good backup"}');

// ---------------------------------------------------------------------------
// Happy path, ordering, events (real StorageLive + real ObjectStoreLocal)
// ---------------------------------------------------------------------------

test("BackupsLive over real Storage + local ObjectStore: ordered puts, restorable object, one event per phase", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-backup-"));
  try {
    const run = collector();
    const deps = Layer.mergeAll(run.deps, ConfigTest({ objectStore: { localDir: join(dir, "objects") } }));
    const storage = StorageLive({ file: join(dir, "app.sqlite"), schema, appId: "app" });
    // `provideMerge`, not a second `mergeAll`: the writer must be built once, shared by test and service.
    const layer = BackupsLive.pipe(
      Layer.provideMerge(Layer.mergeAll(storage, makeObjectStoreLocal())),
      Layer.provide(deps),
    );

    const { manifest, latest, object, objectManifest } = await Effect.runPromise(
      Effect.gen(function* () {
        const db = yield* Storage;
        yield* db.mutation(functions.createGame, { xPlayer: "alice", oPlayer: "bob" });
        const manifest = yield* (yield* Backups).take("app");
        const store = yield* ObjectStore;
        return {
          manifest,
          latest: yield* store.get("apps/app/latest.json"),
          object: yield* store.get(manifest.key),
          objectManifest: yield* store.get(`${manifest.key}.json`),
        };
      }).pipe(Effect.provide(layer)),
    );

    // Manifest shape is byte-compatible with what restore reads.
    assert.equal(manifest.format, 1);
    assert.equal(manifest.appId, "app");
    assert.match(manifest.key, /^apps\/app\/\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{3}Z-id-\d+\.sqlite$/);
    assert.equal(manifest.bytes, object.byteLength);
    assert.equal(manifest.sha256, await sha256(object));
    assert.equal(dec.decode(latest), JSON.stringify(manifest, null, 2));
    assert.deepEqual(objectManifest, latest);

    // The uploaded object is a real, intact copy holding the committed game.
    const restored = join(dir, "restored.sqlite");
    await Deno.writeFile(restored, object);
    integrityCheck(restored);
    const db = inspect(restored);
    try {
      const tables = db.all("SELECT name FROM sqlite_master WHERE type = 'table'").map((r) => r.name);
      assert.ok(tables.length > 0);
      const games = db.get(`SELECT count(*) AS n FROM ${tables.find((t) => String(t).includes("games"))}`);
      assert.equal(games?.n, 1);
    } finally {
      db.close();
    }

    const events = backupEvents(run.events);
    // The store records each transfer as it settles; Backups records the phase that issued it right after.
    assert.deepEqual(
      events.map((e) => [e.emitter, e.phase, e.key]),
      [
        ["backups", "snapshot", undefined],
        ["backups", "verify", undefined],
        ["backups", "hash", undefined],
        ["object-store", "put-object", manifest.key],
        ["backups", "put-object", manifest.key],
        ["object-store", "put-object", `${manifest.key}.json`],
        ["backups", "put-manifest", `${manifest.key}.json`],
        ["object-store", "put-object", "apps/app/latest.json"],
        ["backups", "publish-latest", "apps/app/latest.json"],
      ],
    );
    // One run; exactly one Backups record per phase; one transfer per object written, one attempt each.
    assert.equal(new Set(events.map((e) => e.runId)).size, 1);
    assert.ok(events[0].runId !== undefined && events[0].runId !== events[0].traceId);
    const phases = events.filter((e) => e.emitter === "backups").map((e) => e.phase);
    assert.deepEqual(phases, ["snapshot", "verify", "hash", "put-object", "put-manifest", "publish-latest"]);
    const transfers = events.filter((e) => e.emitter === "object-store");
    assert.deepEqual(transfers.map((e) => e.attempts), [1, 1, 1]);
    // Each transfer links to the phase that issued it by (runId, key).
    for (const transfer of transfers) {
      assert.equal(
        events.filter((e) => e.emitter === "backups" && e.runId === transfer.runId && e.key === transfer.key)
          .length,
        1,
        transfer.key,
      );
    }
    assert.ok(events.every((e) => e.outcome === "ok" && e.level === "info" && e.durationMs >= 0));
    // Every event of the run, the store's included, joins on appId and one traceId.
    assert.ok(events.every((e) => e.appId === "app"));
    assert.equal(new Set(events.map((e) => e.traceId)).size, 1);
    const hash = events.find((e) => e.phase === "hash")!;
    assert.equal(hash.bytes, manifest.bytes);
    assert.equal(hash.sha256, manifest.sha256);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("take keeps a caller's traceId instead of minting one", async () => {
  const store = stubStore();
  const { runner, events } = runnerOver(store);
  await Effect.runPromise(runner.take("app", stubStorage()).pipe(Effect.annotateLogs({ traceId: "req-42" })));
  assert.ok(backupEvents(events).every((e) => e.traceId === "req-42"));
});

test("the temp directory is removed after a successful run", async () => {
  const storage = stubStorage();
  const { runner } = runnerOver(stubStore());
  await Effect.runPromise(runner.take("app", storage));
  assert.equal(storage.paths.length, 1);
  assert.ok(!existsSync(dirname(storage.paths[0])));
});

// ---------------------------------------------------------------------------
// Upload failures never advance latest.json
// ---------------------------------------------------------------------------

test("a put failure on the snapshot object leaves latest.json unchanged", async () => {
  const store = stubStore({ "apps/app/latest.json": OLD_LATEST });
  store.failWhen((key) => key.endsWith(".sqlite"));
  const storage = stubStorage();
  const { runner, events } = runnerOver(store);

  const error = failureOf(await Effect.runPromiseExit(runner.take("app", storage)));
  assert.ok(error instanceof BackupError);
  assert.equal(error.phase, "put-object");
  assert.equal(error.appId, "app");
  assert.ok(error.cause instanceof ObjectStoreError);

  assert.deepEqual(store.objects.get("apps/app/latest.json"), OLD_LATEST);
  assert.equal(store.puts.length, 1, "nothing is uploaded after the failed object put");
  assert.deepEqual([...store.objects.keys()], ["apps/app/latest.json"]);
  assert.ok(!existsSync(dirname(storage.paths[0])));
  // The failed phase is recorded once, by Backups (this stub store records no transfers).
  assert.deepEqual(backupEvents(events).map((e) => [e.phase, e.outcome]), [
    ["snapshot", "ok"],
    ["verify", "ok"],
    ["hash", "ok"],
    ["put-object", "error"],
  ]);
  assert.equal(backupEvents(events)[3].errorTag, "ObjectStoreError");

  // The guard was released: a retry with a healthy store succeeds and only then advances latest.json.
  store.failWhen(() => false);
  const manifest = await Effect.runPromise(runner.take("app", storage));
  assert.equal(dec.decode(store.objects.get("apps/app/latest.json")), JSON.stringify(manifest, null, 2));
});

test("a put failure on the manifest leaves latest.json unchanged and records put-manifest", async () => {
  const store = stubStore({ "apps/app/latest.json": OLD_LATEST });
  store.failWhen((key) => key.endsWith(".sqlite.json"));
  const { runner, events } = runnerOver(store);

  const error = failureOf(await Effect.runPromiseExit(runner.take("app", stubStorage())));
  assert.ok(error instanceof BackupError);
  assert.equal(error.phase, "put-manifest");
  assert.deepEqual(store.objects.get("apps/app/latest.json"), OLD_LATEST);
  assert.ok(!store.puts.includes("apps/app/latest.json"));

  const failed = backupEvents(events).at(-1)!;
  assert.equal(failed.phase, "put-manifest");
  assert.equal(failed.outcome, "error");
  assert.equal(failed.level, "error");
  assert.equal(failed.errorTag, "ObjectStoreError");
});

test("a publish-latest failure is reported at that phase", async () => {
  const store = stubStore({ "apps/app/latest.json": OLD_LATEST });
  store.failWhen((key) => key.endsWith("latest.json"));
  const { runner } = runnerOver(store);
  const error = failureOf(await Effect.runPromiseExit(runner.take("app", stubStorage())));
  assert.ok(error instanceof BackupError);
  assert.equal(error.phase, "publish-latest");
  assert.deepEqual(store.objects.get("apps/app/latest.json"), OLD_LATEST);
});

// ---------------------------------------------------------------------------
// Snapshot and verify failures
// ---------------------------------------------------------------------------

test("a corrupt snapshot fails verify with SnapshotIntegrityError, unwrapped, and uploads nothing", async () => {
  const store = stubStore();
  const storage = stubStorage({ kind: "garbage" });
  const { runner, events } = runnerOver(store);

  const error = failureOf(await Effect.runPromiseExit(runner.take("app", storage)));
  assert.ok(error instanceof SnapshotIntegrityError);
  assert.equal(store.puts.length, 0);
  assert.ok(!existsSync(dirname(storage.paths[0])));
  const verify = backupEvents(events).find((e) => e.phase === "verify")!;
  assert.equal(verify.outcome, "error");
  assert.equal(verify.errorTag, "SnapshotIntegrityError");
});

test("a Storage failure becomes BackupError at phase snapshot", async () => {
  const store = stubStore();
  const storage = stubStorage({ kind: "fail" });
  const { runner, events } = runnerOver(store);
  const error = failureOf(await Effect.runPromiseExit(runner.take("app", storage)));
  assert.ok(error instanceof BackupError);
  assert.equal(error.phase, "snapshot");
  assert.ok(error.cause instanceof StorageError);
  assert.equal(store.puts.length, 0);
  assert.ok(!existsSync(dirname(storage.paths[0])));
  assert.deepEqual(backupEvents(events).map((e) => [e.phase, e.errorTag]), [["snapshot", "StorageError"]]);
});

test("a temp-directory failure fails take at phase snapshot and still emits a snapshot event", async () => {
  const store = stubStore();
  const storage = stubStorage();
  const { runner, events } = runnerOver(store);
  const previous = Deno.env.get("TMPDIR");
  // `Deno.makeTempDir` creates under `TMPDIR`; a missing parent makes it fail.
  Deno.env.set("TMPDIR", join(tmpdir(), `mansio-missing-${crypto.randomUUID()}`));
  let exit: Exit.Exit<Manifest, unknown>;
  try {
    exit = await Effect.runPromiseExit(runner.take("app", storage));
  } finally {
    if (previous === undefined) Deno.env.delete("TMPDIR");
    else Deno.env.set("TMPDIR", previous);
  }
  const error = failureOf(exit);
  assert.ok(error instanceof BackupError);
  assert.equal(error.phase, "snapshot");
  assert.equal(storage.paths.length, 0, "the snapshot was never attempted");
  assert.equal(store.puts.length, 0);
  const trace = backupEvents(events);
  assert.deepEqual(trace.map((e) => [e.phase, e.outcome, e.level, e.errorTag]), [
    ["snapshot", "error", "error", "BackupError"],
  ]);
  assert.equal(trace[0].appId, "app");
  // The guard was released: the next take is admitted and completes.
  await Effect.runPromise(runner.take("app", stubStorage()));
});

test("a defect in Storage still releases the guard and removes the temp directory", async () => {
  const store = stubStore();
  const dying = stubStorage({ kind: "die" });
  const { runner, events } = runnerOver(store);
  const exit = await Effect.runPromiseExit(runner.take("app", dying));
  assert.ok(Exit.isFailure(exit) && Cause.isDie(exit.cause));
  assert.ok(!existsSync(dirname(dying.paths[0])));
  assert.equal(backupEvents(events)[0].errorTag, "Defect");
  await Effect.runPromise(runner.take("app", stubStorage()));
});

// ---------------------------------------------------------------------------
// Interruption
// ---------------------------------------------------------------------------

test("interrupting take mid-upload releases the per-app guard and removes the temp directory", async () => {
  const store = stubStore({ "apps/app/latest.json": OLD_LATEST });
  const storage = stubStorage();
  const { runner, events } = runnerOver(store);

  await Effect.runPromise(Effect.gen(function* () {
    const entered = yield* Deferred.make<void>();
    store.hangWhen((key) => key.endsWith(".sqlite"), entered);
    const fiber = yield* Effect.fork(runner.take("app", storage));
    yield* Deferred.await(entered);

    // While parked in the upload, the guard is held and the snapshot is on disk.
    const temp = dirname(storage.paths[0]);
    assert.ok(existsSync(join(temp, "snapshot.sqlite")));
    const blocked = yield* Effect.exit(runner.take("app", storage));
    assert.ok(Exit.isFailure(blocked));
    const failure = Cause.failureOption(blocked.cause);
    assert.ok(Option.isSome(failure) && failure.value instanceof BackupAlreadyRunning);

    const exit = yield* Fiber.interrupt(fiber);
    assert.ok(Exit.isFailure(exit) && Cause.isInterruptedOnly(exit.cause));
    assert.ok(!existsSync(temp), "temp directory removed on interruption");
  }));

  assert.deepEqual(store.objects.get("apps/app/latest.json"), OLD_LATEST);
  // The stall is visible: the run's trace ends in an interrupted put-object (the stub store emits nothing).
  const trace = backupEvents(events);
  assert.deepEqual(trace.map((e) => [e.phase, e.outcome]), [
    ["snapshot", "ok"],
    ["verify", "ok"],
    ["hash", "ok"],
    ["put-object", "error"],
  ]);
  assert.equal(trace[3].errorTag, "Interrupted");
  assert.equal(trace[3].level, "warn");
  assert.equal(trace[3].emitter, "backups");
  // Whether the object was written is the store's transfer record to say; this phase record makes no claim.
  assert.equal(trace[3].errorDetail, undefined);
  assert.match(trace[3].key ?? "", /^apps\/app\/.*\.sqlite$/);
  // Guard released: the next take is admitted and completes.
  store.unhang();
  const manifest = await Effect.runPromise(runner.take("app", storage));
  assert.equal(manifest.appId, "app");
});

test("interrupted upload over the real ObjectStoreLocal records the phase and one interrupted transfer", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-backup-interrupt-"));
  const writeGate = Promise.withResolvers<void>();
  try {
    const run = collector();
    const writing = Promise.withResolvers<void>();
    const store = makeObjectStoreLocal({
      fs: {
        writeFile: async (path, bytes) => {
          if (path.includes(".sqlite.") && !path.includes(".sqlite.json")) {
            writing.resolve();
            await writeGate.promise;
          }
          await Deno.writeFile(path, bytes);
        },
      },
    }).pipe(Layer.provide(Layer.mergeAll(run.deps, ConfigTest({ objectStore: { localDir: dir } }))));
    await Effect.runPromise(
      Effect.gen(function* () {
        const runner = yield* makeBackupRunner();
        const fiber = yield* Effect.fork(runner.take("app", stubStorage()));
        yield* Effect.promise(() => writing.promise);
        yield* Fiber.interrupt(fiber);
      }).pipe(Effect.provide(Layer.mergeAll(store, run.deps))),
    );
    const puts = backupEvents(run.events).filter((e) => e.phase === "put-object");
    assert.equal(puts.length, 2, "one phase record from Backups and one transfer record from the store");
    const phase = puts.find((e) => e.emitter === "backups");
    const transfer = puts.find((e) => e.emitter === "object-store");
    assert.ok(phase && transfer);
    assert.equal(phase.errorTag, "Interrupted");
    assert.equal(phase.errorDetail, undefined);
    // The store's transfer record is written from a finalizer: it says the put was interrupted after
    // one request, and that the object may or may not have been written.
    assert.equal(transfer.outcome, "error");
    assert.equal(transfer.errorTag, "Interrupted");
    assert.equal(transfer.level, "warn");
    assert.equal(transfer.attempts, 1);
    assert.deepEqual(transfer.errorDetail, { written: "unknown" });
    assert.equal(transfer.runId, phase.runId);
    assert.equal(transfer.key, phase.key);
    assert.ok(!existsSync(join(dir, "apps/app/latest.json")));
  } finally {
    writeGate.resolve();
    // Let the orphaned write settle before its directory goes.
    await new Promise((resolve) => setTimeout(resolve, 10));
    rmSync(dir, { recursive: true, force: true });
  }
});

test("interrupting take during the snapshot releases the guard, removes the temp dir, records the phase", async () => {
  const store = stubStore();
  const { runner, events } = runnerOver(store);
  await Effect.runPromise(Effect.gen(function* () {
    const entered = yield* Deferred.make<void>();
    const release = yield* Deferred.make<void>();
    const storage = stubStorage({ kind: "gate", entered, release });
    const fiber = yield* Effect.fork(runner.take("app", storage));
    yield* Deferred.await(entered);
    yield* Fiber.interrupt(fiber);
    assert.ok(!existsSync(dirname(storage.paths[0])));
    yield* runner.take("app", stubStorage());
  }));
  const interrupted = backupEvents(events)[0];
  assert.equal(interrupted.phase, "snapshot");
  assert.equal(interrupted.outcome, "error");
  assert.equal(interrupted.errorTag, "Interrupted");
  assert.equal(interrupted.level, "warn");
  assert.equal(
    store.puts.filter((k) => k.endsWith("latest.json")).length,
    1,
    "only the second run published",
  );
});

// ---------------------------------------------------------------------------
// The per-app guard
// ---------------------------------------------------------------------------

test("two concurrent takes for the same app produce exactly one BackupAlreadyRunning", async () => {
  const store = stubStore();
  const { runner } = runnerOver(store);
  const exits = await Effect.runPromise(Effect.gen(function* () {
    const entered = yield* Deferred.make<void>();
    const release = yield* Deferred.make<void>();
    const first = yield* Effect.fork(
      Effect.exit(runner.take("app", stubStorage({ kind: "gate", entered, release }))),
    );
    yield* Deferred.await(entered);
    const second = yield* Effect.exit(runner.take("app", stubStorage()));
    yield* Deferred.succeed(release, undefined);
    return [yield* Fiber.join(first), second] as const;
  }));
  const rejected = exits.filter((exit) =>
    Exit.isFailure(exit) &&
    Option.getOrUndefined(Cause.failureOption(exit.cause)) instanceof BackupAlreadyRunning
  );
  assert.equal(rejected.length, 1);
  assert.ok(Exit.isSuccess(exits[0]));
  const error = failureOf(exits[1]) as BackupAlreadyRunning;
  assert.equal(error.appId, "app");
});

test("concurrent takes for different apps both run", async () => {
  const store = stubStore();
  const { runner } = runnerOver(store);
  const manifests: Manifest[] = await Effect.runPromise(
    Effect.all([runner.take("a", stubStorage()), runner.take("b", stubStorage())], { concurrency: 2 }),
  );
  assert.deepEqual(manifests.map((m) => m.appId), ["a", "b"]);
  assert.ok(store.objects.has("apps/a/latest.json") && store.objects.has("apps/b/latest.json"));
});

test("the guard is a race-free claim: N simultaneous takes while one is in flight admit exactly one", async () => {
  const store = stubStore();
  const { runner } = runnerOver(store);
  const exits = await Effect.runPromise(Effect.gen(function* () {
    const entered = yield* Deferred.make<void>();
    const release = yield* Deferred.make<void>();
    // One shared gated storage: whichever take is admitted parks in its snapshot until released.
    const storage = stubStorage({ kind: "gate", entered, release });
    const fibers = yield* Effect.forEach(
      Array.from({ length: 8 }),
      () => Effect.fork(Effect.exit(runner.take("app", storage))),
    );
    yield* Deferred.await(entered);
    // Every other fiber has started and been rejected before the admitted one is released.
    yield* Effect.sleep("20 millis");
    yield* Deferred.succeed(release, undefined);
    return yield* Fiber.joinAll(fibers);
  }));
  const busy = exits.filter((exit) =>
    Exit.isFailure(exit) &&
    Option.getOrUndefined(Cause.failureOption(exit.cause)) instanceof BackupAlreadyRunning
  ).length;
  assert.equal(exits.filter(Exit.isSuccess).length, 1);
  assert.equal(busy, exits.length - 1);
  assert.equal(store.puts.filter((k) => k.endsWith("latest.json")).length, 1);
});

// ---------------------------------------------------------------------------
// Accounting through the event log (item 35, ADR 0006)
// ---------------------------------------------------------------------------

test("ingested, two backups under one caller trace with a retried upload count as 2 runs, 12 phases, 6 transfers, 1 retry", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-backup-accounting-"));
  try {
    const run = collector();
    // The very first request (the first run's snapshot-object PUT) is throttled once, then everything succeeds.
    let requests = 0;
    const s3 = (() => {
      requests += 1;
      return Promise.resolve(
        requests === 1 ? new Response("SlowDown", { status: 503 }) : new Response(null, { status: 200 }),
      );
    }) as unknown as typeof fetch;
    const config = ConfigTest({
      objectStore: {
        s3BaseUrl: "https://s3.example.test/bucket",
        accessKeyId: "AKIDEXAMPLE",
        secretAccessKey: "secret-example-key",
        region: "us-east-1",
      },
    });
    const store = makeObjectStoreS3({ fetch: s3, retryBase: "1 millis" }).pipe(
      Layer.provide(Layer.mergeAll(config, run.deps)),
    );
    await Effect.runPromise(
      Effect.gen(function* () {
        const runner = yield* makeBackupRunner();
        yield* runner.take("app", stubStorage());
        yield* runner.take("app", stubStorage());
      }).pipe(Effect.annotateLogs({ traceId: "nightly" }), Effect.provide(Layer.mergeAll(store, run.deps))),
    );
    assert.equal(requests, 7, "six PUTs, one of them twice");

    // Through the real JSONL → DuckDB path, as item 23's queries will see them.
    const logDir = join(dir, "logs");
    const source = join(logDir, "server/2026-10-10/events-12-4242-001.jsonl");
    await Deno.mkdir(dirname(source), { recursive: true });
    await Deno.writeTextFile(source, run.events.map((event) => toJsonLine(event) + "\n").join(""));
    const dbPath = join(dir, "events.duckdb");
    const report = await Effect.runPromise(
      Effect.scoped(Effect.flatMap(makeEventLogIngest(), (service) => service.ingest)).pipe(
        Effect.provide(Layer.merge(
          ConfigTest({ logging: { dir: logDir }, eventLog: { duckdbPath: dbPath } }),
          Layer.succeed(EventSink, { emit: () => Effect.void }),
        )),
      ),
    );
    assert.equal(report.linesIngested, 18);

    const instance = await DuckDBInstance.create(dbPath);
    try {
      const connection = await instance.connect();
      try {
        const read = async (sql: string) => (await connection.runAndReadAll(sql)).getRowObjects();
        const [totals] = await read(`SELECT
            count(DISTINCT run_id) AS runs,
            count(*) FILTER (WHERE emitter = 'backups') AS phases,
            count(*) FILTER (WHERE emitter = 'backups' AND phase = 'publish-latest' AND outcome = 'ok') AS completed,
            count(*) FILTER (WHERE emitter = 'object-store') AS transfers,
            sum(attempts - 1) FILTER (WHERE emitter = 'object-store') AS retries,
            count(*) FILTER (WHERE phase = 'put-object') AS put_object_rows
          FROM backup_events`);
        // `put_object_rows` is what a query blind to `emitter` would count as uploads: 8, for 6 objects.
        assert.deepEqual(totals, {
          runs: 2n,
          phases: 12n,
          completed: 2n,
          transfers: 6n,
          retries: 1n,
          put_object_rows: 8n,
        });
        // The trace cannot tell the two backups apart; run_id can.
        const [traces] = await read(
          "SELECT count(DISTINCT p.trace_id) AS n FROM backup_events b JOIN platform_events p USING (event_id)",
        );
        assert.equal(traces.n, 1n);
        // The retried transfer links to exactly the phase that issued it.
        const retried = await read(`SELECT b.phase, t.attempts, t.outcome
          FROM backup_events t JOIN backup_events b
            ON b.run_id = t.run_id AND b.key = t.key AND b.emitter = 'backups'
          WHERE t.emitter = 'object-store' AND t.attempts > 1`);
        assert.deepEqual(retried, [{ phase: "put-object", attempts: 2, outcome: "ok" }]);
        const [perRun] = await read(`SELECT count(*) AS runs FROM (
            SELECT run_id FROM backup_events GROUP BY run_id
            HAVING count(*) FILTER (WHERE emitter = 'backups') = 6
               AND count(*) FILTER (WHERE emitter = 'object-store') = 3)`);
        assert.equal(perRun.runs, 2n);
      } finally {
        connection.closeSync();
      }
    } finally {
      instance.closeSync();
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// verifySnapshot / integrityCheck
// ---------------------------------------------------------------------------

test("verifySnapshot accepts a healthy file and rejects a non-database as SnapshotIntegrityError", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mansio-verify-"));
  try {
    const good = join(dir, "good.sqlite");
    writeValidSnapshot(good);
    await Effect.runPromise(verifySnapshot(good));
    integrityCheck(good);

    const bad = join(dir, "bad.sqlite");
    writeFileSync(bad, "x".repeat(4096));
    const error = failureOf(await Effect.runPromiseExit(verifySnapshot(bad)));
    assert.ok(error instanceof SnapshotIntegrityError);
    assert.equal(error.file, bad);
    // The deprecated sync wrapper throws the tagged failure, not an untyped Error.
    assert.throws(() => integrityCheck(bad), (error) => {
      assert.ok(error instanceof SnapshotIntegrityError);
      assert.equal(error.file, bad);
      return true;
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// Real S3 (SeaweedFS): gated on S3_INTEGRATION=1
// ---------------------------------------------------------------------------

const s3Enabled = Deno.env.get("S3_INTEGRATION") === "1";
const s3Skip = s3Enabled ? false : "S3_INTEGRATION=1 is not set; real-S3 backup round trip skipped";
const s3Endpoint = (Deno.env.get("S3_BASE_URL") ?? "http://localhost:8333").replace(/\/$/, "");
const s3BucketUrl = s3Endpoint !== "" && new URL(s3Endpoint).pathname !== "/"
  ? s3Endpoint
  : `${s3Endpoint}/${Deno.env.get("S3_BUCKET") ?? "mansio-test"}`;
const s3Creds = {
  accessKeyId: Deno.env.get("AWS_ACCESS_KEY_ID") ?? "mansiodevkey",
  secretAccessKey: Deno.env.get("AWS_SECRET_ACCESS_KEY") ?? "mansiodevsecret",
  region: Deno.env.get("AWS_REGION") ?? "us-east-1",
};
/** A per-run app id, so the run's `apps/<id>/` prefix is unique and can be deleted afterwards. */
const s3AppId = `it-backup-${Date.now()}-${crypto.randomUUID().slice(0, 8)}`;
const s3Written = new Set<string>();

after(async () => {
  if (!s3Enabled || s3Written.size === 0) return;
  const client = new AwsClient({ ...s3Creds, service: "s3", retries: 0 });
  for (const key of s3Written) {
    const response = await client.fetch(`${s3BucketUrl}/${key}`, { method: "DELETE" });
    await response.body?.cancel();
  }
});

test("real S3: backup round trip over ObjectStoreS3 publishes latest.json last and restores", {
  skip: s3Skip,
}, async () => {
  const run = collector();
  const config = ConfigTest({ objectStore: { s3BaseUrl: s3BucketUrl, ...s3Creds } });
  const store = makeObjectStoreS3().pipe(Layer.provide(Layer.mergeAll(config, run.deps)));
  const result = await Effect.runPromise(
    Effect.gen(function* () {
      const runner = yield* makeBackupRunner();
      const manifest = yield* runner.take(s3AppId, stubStorage());
      for (const key of [manifest.key, `${manifest.key}.json`, `apps/${s3AppId}/latest.json`]) {
        s3Written.add(key);
      }
      const s3 = yield* ObjectStore;
      return {
        manifest,
        latest: yield* s3.get(`apps/${s3AppId}/latest.json`),
        object: yield* s3.get(manifest.key),
      };
    }).pipe(Effect.provide(Layer.mergeAll(store, run.deps))),
  );
  assert.equal(dec.decode(result.latest), JSON.stringify(result.manifest, null, 2));
  assert.equal(await sha256(result.object), result.manifest.sha256);
  const puts = backupEvents(run.events).filter((e) => e.emitter === "object-store").map((e) => e.key);
  assert.deepEqual(puts, [result.manifest.key, `${result.manifest.key}.json`, `apps/${s3AppId}/latest.json`]);
  const dir = mkdtempSync(join(tmpdir(), "mansio-s3-restore-"));
  try {
    const file = join(dir, "restored.sqlite");
    await Deno.writeFile(file, result.object);
    await Effect.runPromise(verifySnapshot(file));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

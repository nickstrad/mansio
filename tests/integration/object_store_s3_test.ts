/**
 * Real-S3 integration test for the `ObjectStoreS3` layer (plan item 18).
 *
 * Unlike `tests/unit/object_store_test.ts` (stub `fetch`), every request here goes over the network to a
 * live S3-compatible endpoint — locally, SeaweedFS from `docker-compose.yml` (`make up && make s3-init`).
 * The store under test is built exactly as the server builds it: `makeObjectStoreS3()` over `ConfigLive`
 * (item 08), with the global `fetch`.
 *
 * Run:
 *   S3_INTEGRATION=1 S3_BASE_URL=http://localhost:8333 AWS_ACCESS_KEY_ID=mansiodevkey \
 *   AWS_SECRET_ACCESS_KEY=mansiodevsecret AWS_REGION=us-east-1 \
 *   deno test -A tests/integration/object_store_s3_test.ts
 *
 * Gated on `S3_INTEGRATION=1`: without it every test is skipped, so a machine with no S3 never fails.
 * `S3_BUCKET` (default `mansio-test`) names the bucket. The layer's base URL *includes* the bucket, so the
 * test composes `${S3_BASE_URL}/${S3_BUCKET}` unless `S3_BASE_URL` already carries a path.
 *
 * Every run writes under its own `it-s3/<timestamp>-<random>/` prefix and deletes what it wrote. The
 * `ObjectStore` service has no delete, so deletion goes through a raw signed `DELETE`.
 */
import { after, test } from "node:test";
import assert from "node:assert/strict";
import { AwsClient } from "aws4fetch";
import { Cause, Effect, Exit, Layer, Option } from "effect";
import { ObjectStoreError } from "../../core/errors.ts";
import type { EventRecord } from "../../core/events.ts";
import { ConfigLive } from "../../platform/config.ts";
import { makeObjectStoreS3 } from "../../platform/object_store.ts";
import { Config, EventSink, IdGen, ObjectStore } from "../../platform/services.ts";

const enabled = Deno.env.get("S3_INTEGRATION") === "1";
const skip = enabled ? false : "S3_INTEGRATION=1 is not set; real-S3 tests skipped";
if (!enabled) console.warn(`SKIP object_store_s3_test: ${skip}`);

const enc = new TextEncoder();
const dec = new TextDecoder();

const bucket = Deno.env.get("S3_BUCKET") ?? "mansio-test";
const accessKeyId = Deno.env.get("AWS_ACCESS_KEY_ID") ?? "";
const secretAccessKey = Deno.env.get("AWS_SECRET_ACCESS_KEY") ?? "";
const region = Deno.env.get("AWS_REGION") ?? "us-east-1";
const endpoint = (Deno.env.get("S3_BASE_URL") ?? "").replace(/\/$/, "");
const hasPath = endpoint !== "" && new URL(endpoint).pathname !== "/";
const bucketUrl = hasPath ? endpoint : `${endpoint}/${bucket}`;
/** Same endpoint, a bucket that does not exist. Only ever read from, so nothing can be created. */
const missingBucketUrl = `${endpoint}/mansio-no-such-bucket-${crypto.randomUUID().slice(0, 8)}`;

/** Unique per run so reruns (and concurrent runs) never collide. */
const prefix = `it-s3/${Date.now()}-${crypto.randomUUID().slice(0, 8)}`;
const written = new Set<string>();

interface Harness {
  readonly layer: Layer.Layer<ObjectStore, unknown>;
  readonly events: EventRecord[];
}

/**
 * The real S3 layer over `ConfigLive` (env), with `objectStore` fields overridden where a case needs a
 * different bucket or credentials. Events are captured in memory; ids are sequential.
 */
function harness(override: Partial<{ s3BaseUrl: string; secretAccessKey: string }> = {}): Harness {
  const events: EventRecord[] = [];
  let n = 0;
  const config = Layer.effect(
    Config,
    Effect.map(
      Config,
      (c) => ({
        ...c,
        objectStore: { ...c.objectStore, s3BaseUrl: bucketUrl, ...override },
      }),
    ),
  ).pipe(Layer.provide(ConfigLive));
  const deps = Layer.mergeAll(
    config,
    Layer.succeed(EventSink, { emit: (event) => Effect.sync(() => void events.push(event)) }),
    Layer.succeed(IdGen, { next: Effect.sync(() => `id-${++n}`) }),
  );
  return { layer: Layer.provide(makeObjectStoreS3(), deps), events };
}

function exec<A, E>(h: Harness, program: Effect.Effect<A, E, ObjectStore>): Promise<Exit.Exit<A, unknown>> {
  return Effect.runPromiseExit(Effect.provide(program, h.layer));
}

function valueOf<A>(exit: Exit.Exit<A, unknown>): A {
  assert.ok(
    Exit.isSuccess(exit),
    `expected success, got ${Exit.isFailure(exit) ? Cause.pretty(exit.cause) : ""}`,
  );
  return exit.value;
}

function failureOf<A>(exit: Exit.Exit<A, unknown>): unknown {
  assert.ok(Exit.isFailure(exit), "expected a failure");
  const failure = Cause.failureOption(exit.cause);
  assert.ok(Option.isSome(failure), "expected a typed failure, not a defect");
  return failure.value;
}

const put = (key: string, bytes: Uint8Array) => {
  written.add(key);
  return Effect.flatMap(ObjectStore, (s) => s.put(key, bytes));
};
const get = (key: string) => Effect.flatMap(ObjectStore, (s) => s.get(key));

const signer = new AwsClient({ accessKeyId, secretAccessKey, service: "s3", region, retries: 0 });
const objectUrl = (key: string) => `${bucketUrl}/${key.split("/").map(encodeURIComponent).join("/")}`;

/** Raw signed DELETE — the service interface has none. S3 answers 204 whether or not the key existed. */
async function rawDelete(key: string): Promise<number> {
  const response = await signer.fetch(objectUrl(key), { method: "DELETE" });
  await response.body?.cancel();
  return response.status;
}

after(async () => {
  if (!enabled) return;
  await Promise.allSettled([...written].map(rawDelete));
});

test("s3 real: put then get round-trips arbitrary bytes", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/roundtrip.bin`;
  const payload = Uint8Array.from({ length: 4096 }, (_, i) => (i * 31 + 7) & 0xff);
  valueOf(await exec(h, put(key, payload)));
  const back = valueOf(await exec(h, get(key)));
  assert.equal(back.byteLength, payload.byteLength);
  assert.deepEqual(back, payload);
});

test("s3 real: put and get emit one event each with key, bytes and outcome", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/events.bin`;
  valueOf(await exec(h, put(key, enc.encode("event payload"))));
  valueOf(await exec(h, get(key)));
  assert.equal(h.events.length, 2);
  const [putEvent, getEvent] = h.events;
  assert.equal(putEvent.kind, "backup");
  assert.equal(getEvent.kind, "lifecycle");
  assert.equal(putEvent.level, "info");
  assert.equal(getEvent.level, "info");
  assert.ok(putEvent.kind === "backup" && putEvent.key === key && putEvent.outcome === "ok");
  assert.ok(putEvent.kind === "backup" && putEvent.bytes === "event payload".length);
});

test("s3 real: nested key round-trips", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/tic-tac-toe/2026-10-10/snap_v1.sqlite`;
  const payload = enc.encode("nested");
  valueOf(await exec(h, put(key, payload)));
  assert.equal(dec.decode(valueOf(await exec(h, get(key)))), "nested");
});

test("s3 real: overwriting a key replaces the object", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/latest.json`;
  valueOf(await exec(h, put(key, enc.encode("first version, longer"))));
  assert.equal(dec.decode(valueOf(await exec(h, get(key)))), "first version, longer");
  valueOf(await exec(h, put(key, enc.encode("second"))));
  assert.equal(dec.decode(valueOf(await exec(h, get(key)))), "second");
});

test("s3 real: an empty object round-trips", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/empty.bin`;
  valueOf(await exec(h, put(key, new Uint8Array(0))));
  assert.equal(valueOf(await exec(h, get(key))).byteLength, 0);
});

test("s3 real: get after delete maps 404 to ObjectStoreError", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/to-delete.bin`;
  valueOf(await exec(h, put(key, enc.encode("soon gone"))));
  assert.equal(dec.decode(valueOf(await exec(h, get(key)))), "soon gone");
  assert.equal(await rawDelete(key), 204);
  const error = failureOf(await exec(h, get(key)));
  assert.ok(error instanceof ObjectStoreError);
  assert.equal(error.operation, "get");
  assert.equal(error.key, key);
  assert.equal(error.status, 404);
  assert.match(error.detail ?? "", /NoSuchKey/);
});

test("s3 real: get of a key that never existed is a 404 ObjectStoreError", { skip }, async () => {
  const h = harness();
  const key = `${prefix}/never-written.bin`;
  const error = failureOf(await exec(h, get(key)));
  assert.ok(error instanceof ObjectStoreError);
  assert.equal(error.status, 404);
  assert.match(error.detail ?? "", /NoSuchKey/);
  const [event] = h.events;
  assert.equal(event.level, "error");
});

test("s3 real: a nonexistent bucket maps to a 404 NoSuchBucket ObjectStoreError", { skip }, async () => {
  const h = harness({ s3BaseUrl: missingBucketUrl });
  const error = failureOf(await exec(h, get(`${prefix}/anything.bin`)));
  assert.ok(error instanceof ObjectStoreError);
  assert.equal(error.operation, "get");
  assert.equal(error.status, 404);
  assert.match(error.detail ?? "", /NoSuchBucket/);
});

test("s3 real: a wrong secret is a 403 ObjectStoreError, not retried into success", { skip }, async () => {
  const h = harness({ secretAccessKey: "definitely-not-the-secret" });
  const key = `${prefix}/forbidden.bin`;
  const error = failureOf(await exec(h, put(key, enc.encode("nope"))));
  assert.ok(error instanceof ObjectStoreError);
  assert.equal(error.operation, "put");
  assert.equal(error.status, 403);
  assert.match(error.detail ?? "", /SignatureDoesNotMatch|AccessDenied|InvalidAccessKeyId/);
  // The object was never created: the correctly-signed store sees a 404.
  const verify = failureOf(await exec(harness(), get(key)));
  assert.ok(verify instanceof ObjectStoreError);
  assert.equal(verify.status, 404);
});

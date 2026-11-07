import { describe, expect, it } from "vitest";
import { decodeArchitecture } from "c4-sdk/core";
import { liftDefaultId } from "../src/catalog.ts";
import { chooseDefault } from "../src/routes.ts";
import { loadArchitectures } from "../src/sdk.ts";
import { readJson, readText } from "./fixtures.ts";

const SITE = "https://site.test/mansio/";
const CATALOG_URL = `${SITE}catalog.json`;

/**
 * Serves the site from disk the way the deployed site is laid out: catalog.json is `catalog`, the fixture
 * bundle comes from tests/fixtures/ (it is never published), every other path from public/.
 */
function siteFetch(catalog: string): typeof fetch {
  return (input) => {
    const path = String(input).slice(SITE.length);
    const file = path === "architectures/fixture-third.json"
      ? "tests/fixtures/fixture-third.json"
      : `public/${path}`;
    const body = path === "catalog.json" ? catalog : readText(file);
    return Promise.resolve(new Response(body, { headers: { "content-type": "application/json" } }));
  };
}

async function load(catalog: string) {
  const lifted = liftDefaultId(CATALOG_URL, siteFetch(catalog));
  const entries = await loadArchitectures(CATALOG_URL, new AbortController().signal, lifted.fetch);
  const ids = entries.map(({ entry }) => entry.id);
  return {
    ids,
    ok: entries.map(({ result }) => result.ok),
    defaultId: lifted.defaultId(),
    choice: chooseDefault(ids, lifted.defaultId()),
  };
}

describe("catalog loading", () => {
  it("loads the site catalog and opens mansio-current by default", async () => {
    expect(await load(readText("public/catalog.json"))).toEqual({
      ids: [
        "mansio-current",
        "mansio-lanes-proposal",
        "mansio-planes-proposal",
        "mansio-planes-fable",
        "mansio-go-platform",
      ],
      ok: [true, true, true, true, true],
      defaultId: "mansio-current",
      choice: { ok: true, id: "mansio-current" },
    });
  });

  it("loads a three-entry catalog with no code change, defaulting to its defaultId", async () => {
    // The fixture's defaultId is its LAST entry, so the default is not merely the first entry.
    expect(await load(readText("tests/fixtures/catalog-three.json"))).toEqual({
      ids: ["mansio-current", "mansio-lanes-proposal", "fixture-third"],
      ok: [true, true, true],
      defaultId: "fixture-third",
      choice: { ok: true, id: "fixture-third" },
    });
  });

  it("defaults to the first entry when the catalog has no defaultId", async () => {
    const { entries } = readJson("tests/fixtures/catalog-three.json") as { entries: unknown[] };
    const catalog = JSON.stringify({ entries: [entries[2], entries[0]] });
    expect(await load(catalog)).toEqual({
      ids: ["fixture-third", "mansio-current"],
      ok: [true, true],
      defaultId: undefined,
      choice: { ok: true, id: "fixture-third" },
    });
  });

  it("lifts only defaultId: any other unknown catalog key is still rejected by the SDK", async () => {
    const catalog = JSON.stringify({ ...JSON.parse(readText("public/catalog.json")), extra: true });
    await expect(load(catalog)).rejects.toThrow(/"unrecognized_keys",\s*"keys": \[\s*"extra"/);
  });

  it("passes every other request through untouched", async () => {
    const response = new Response("{}");
    const lifted = liftDefaultId(CATALOG_URL, () => Promise.resolve(response));
    expect(await lifted.fetch(`${SITE}architectures/mansio-current.json`)).toBe(response);
    expect(lifted.defaultId()).toBeUndefined();
  });
});

/** c4-sdk's catalog limit (`createFetchLoader`, default `maxCatalogBytes`) and the SDK's error for it. */
const LIMIT = 262144;
const SIZE_ERROR = "Resource exceeds the 262144-byte limit.";
const CHUNK = 65536;

/**
 * A body stream that counts what is taken from it. `highWaterMark: 0` makes it produce a chunk only when a
 * read is waiting, so `pulls()` is exactly the number of chunks the reader consumed. It ends after `chunks`
 * chunks of `CHUNK` spaces, or never when `chunks` is `Infinity`.
 */
function countedStream(chunks: number) {
  let pulls = 0;
  let cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (pulls === chunks) return controller.close();
      pulls++;
      controller.enqueue(new Uint8Array(CHUNK).fill(0x20));
    },
    cancel() {
      cancelled = true;
    },
  }, { highWaterMark: 0 });
  return { stream, pulls: () => pulls, cancelled: () => cancelled };
}

const catalogResponse = (body: BodyInit | null, init?: ResponseInit) => () =>
  Promise.resolve(new Response(body, init));

describe("the catalog fetch keeps the SDK's size bound while streaming", () => {
  // Hand-worked: LIMIT is 4 chunks. Chunks 1-4 make 262144 bytes, not over the limit; chunk 5 crosses it,
  // so the wrapper reads 5 chunks (327680 bytes = LIMIT + CHUNK), keeps LIMIT + 1 = 262145 of them,
  // cancels the source and hands those bytes on.
  for (
    const [name, chunks, headers] of [
      ["no Content-Length, finite 1 MiB body", 16, {}],
      ["an understated Content-Length", 16, { "content-length": "10" }],
      ["a body that never ends", Infinity, {}],
    ] as const
  ) {
    it(`stops after LIMIT + 1 bytes and cancels the source: ${name}`, async () => {
      const source = countedStream(chunks);
      const lifted = liftDefaultId(CATALOG_URL, catalogResponse(source.stream, { headers }));
      const response = await lifted.fetch(CATALOG_URL);
      expect({ pulls: source.pulls(), cancelled: source.cancelled() }).toEqual({ pulls: 5, cancelled: true });
      expect(source.pulls() * CHUNK).toBeLessThanOrEqual(LIMIT + CHUNK);
      expect((await response.arrayBuffer()).byteLength).toBe(LIMIT + 1);
    });

    it(`the SDK reports its size error promptly: ${name}`, async () => {
      const source = countedStream(chunks);
      const lifted = liftDefaultId(CATALOG_URL, catalogResponse(source.stream, { headers }));
      await expect(loadArchitectures(CATALOG_URL, new AbortController().signal, lifted.fetch))
        .rejects.toThrow(SIZE_ERROR);
      expect({ pulls: source.pulls(), cancelled: source.cancelled() }).toEqual({ pulls: 5, cancelled: true });
    });
  }

  it("leaves a body whose Content-Length is over the limit unread for the SDK to reject", async () => {
    const source = countedStream(16);
    const original = new Response(source.stream, { headers: { "content-length": String(LIMIT + 1) } });
    const lifted = liftDefaultId(CATALOG_URL, () => Promise.resolve(original));
    const response = await lifted.fetch(CATALOG_URL);
    expect(response).toBe(original);
    expect({ bodyUsed: response.bodyUsed, pulls: source.pulls() }).toEqual({ bodyUsed: false, pulls: 0 });
    const again = liftDefaultId(
      CATALOG_URL,
      catalogResponse(countedStream(16).stream, {
        headers: { "content-length": String(LIMIT + 1) },
      }),
    );
    await expect(loadArchitectures(CATALOG_URL, new AbortController().signal, again.fetch))
      .rejects.toThrow(SIZE_ERROR);
  });

  it("lifts defaultId from a catalog of exactly LIMIT bytes; one byte more is the SDK size error", async () => {
    const json = JSON.stringify({
      ...readJson("public/catalog.json") as object,
      defaultId: "mansio-lanes-proposal",
    });
    const padded = (bytes: number) => json + " ".repeat(bytes - new TextEncoder().encode(json).byteLength);
    expect(new TextEncoder().encode(padded(LIMIT)).byteLength).toBe(LIMIT);
    expect(await load(padded(LIMIT))).toEqual({
      ids: [
        "mansio-current",
        "mansio-lanes-proposal",
        "mansio-planes-proposal",
        "mansio-planes-fable",
        "mansio-go-platform",
      ],
      ok: [true, true, true, true, true],
      defaultId: "mansio-lanes-proposal",
      choice: { ok: true, id: "mansio-lanes-proposal" },
    });
    await expect(load(padded(LIMIT + 1))).rejects.toThrow(SIZE_ERROR);
  });

  it("cancels the source and rejects with the abort when the signal aborts mid-stream", async () => {
    const abort = new AbortController();
    let pulls = 0;
    let cancelled: unknown = "not cancelled";
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulls++;
        // First read: part of a catalog. Second read: the server stalls, and the reader aborts meanwhile.
        if (pulls === 1) return controller.enqueue(new TextEncoder().encode('{"entries":'));
        setTimeout(() => abort.abort(), 0);
        return new Promise<void>(() => {});
      },
      cancel(reason) {
        cancelled = reason;
      },
    }, { highWaterMark: 0 });
    const lifted = liftDefaultId(CATALOG_URL, catalogResponse(stream));
    const loading = loadArchitectures(CATALOG_URL, abort.signal, lifted.fetch);
    await expect(loading).rejects.toMatchObject({ name: "AbortError" });
    expect(pulls).toBe(2);
    expect(cancelled).toBe(abort.signal.reason);
  });

  it("rejects at once, cancelling the body, when the signal is already aborted", async () => {
    const abort = new AbortController();
    abort.abort();
    const source = countedStream(1);
    const lifted = liftDefaultId(CATALOG_URL, catalogResponse(source.stream));
    await expect(lifted.fetch(CATALOG_URL, { signal: abort.signal })).rejects.toMatchObject({
      name: "AbortError",
    });
    expect({ pulls: source.pulls(), cancelled: source.cancelled() }).toEqual({ pulls: 0, cancelled: true });
  });

  it("passes invalid UTF-8 through byte for byte, for the SDK to report", async () => {
    const bytes = new Uint8Array([0x7b, 0xff, 0x7d]); // "{", a byte that is never UTF-8, "}"
    const lifted = liftDefaultId(CATALOG_URL, catalogResponse(bytes));
    expect(new Uint8Array(await (await lifted.fetch(CATALOG_URL)).arrayBuffer())).toEqual(bytes);
    await expect(loadArchitectures(CATALOG_URL, new AbortController().signal, lifted.fetch))
      .rejects.toThrow("Resource is not valid UTF-8 JSON.");
  });

  it("passes invalid JSON through unchanged, for the SDK to report", async () => {
    const lifted = liftDefaultId(CATALOG_URL, catalogResponse('{"defaultId":'));
    expect(await (await lifted.fetch(CATALOG_URL)).text()).toBe('{"defaultId":');
    await expect(loadArchitectures(CATALOG_URL, new AbortController().signal, lifted.fetch))
      .rejects.toThrow("Resource is not valid UTF-8 JSON.");
    expect(lifted.defaultId()).toBeUndefined();
  });

  it("passes a failed or body-less catalog response through untouched", async () => {
    for (const original of [new Response("{}", { status: 404 }), new Response(null)]) {
      const lifted = liftDefaultId(CATALOG_URL, () => Promise.resolve(original));
      const response = await lifted.fetch(CATALOG_URL);
      expect(response).toBe(original);
      expect(response.bodyUsed).toBe(false);
    }
  });
});

describe("the three-entry fixture bundle", () => {
  it("decodes with decodeArchitecture and no diagnostics at all", () => {
    const decoded = decodeArchitecture(readJson("tests/fixtures/fixture-third.json"));
    expect(decoded.diagnostics).toEqual([]);
    expect(decoded.ok).toBe(true);
  });
});

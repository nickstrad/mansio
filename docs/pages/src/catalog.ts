/**
 * `catalog.json` is the SDK catalog (`{ entries: [{ id, title, url }] }`) plus one site-level key,
 * `defaultId`: the entry the site opens when the URL names none. The SDK's catalog schema is strict, so the
 * site hands the SDK loader this fetch, which lifts `defaultId` off the catalog response before the SDK
 * reads it. Every other request, and any catalog the SDK would reject anyway, passes through unchanged so
 * the SDK reports it.
 */

/** The SDK fetch loader's catalog size limit. */
const MAX_CATALOG_BYTES = 256 * 1024;

export interface CatalogFetch {
  /** The fetch to give the SDK loader. */
  readonly fetch: typeof fetch;
  /** The `defaultId` value read from the catalog (`undefined` until it is loaded, or if it has none). */
  readonly defaultId: () => unknown;
}

export function liftDefaultId(
  catalogUrl: string,
  fetchResource: typeof fetch = fetch,
): CatalogFetch {
  let defaultId: unknown;
  return {
    defaultId: () => defaultId,
    fetch: async (input, init) => {
      const response = await fetchResource(input, init);
      if (input !== catalogUrl || !response.ok) return response;
      const length = Number(response.headers.get("content-length"));
      if (Number.isFinite(length) && length > MAX_CATALOG_BYTES) return response;
      if (!response.body) return response;
      const bytes = await readAtMost(response.body, MAX_CATALOG_BYTES + 1, init?.signal ?? undefined);
      const status = { status: response.status, statusText: response.statusText };
      const unchanged = () => new Response(bytes, status);
      if (bytes.byteLength > MAX_CATALOG_BYTES) return unchanged();
      let json: unknown;
      try {
        json = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
      } catch {
        return unchanged();
      }
      if (typeof json !== "object" || json === null || Array.isArray(json) || !("defaultId" in json)) {
        return unchanged();
      }
      const { defaultId: lifted, ...catalog } = json as Record<string, unknown>;
      defaultId = lifted;
      return new Response(JSON.stringify(catalog), status);
    },
  };
}

/**
 * Reads `body` until it ends or `limit` bytes have arrived, then cancels it, so an oversized or endless
 * body is never buffered past `limit`. Rejects with the signal's reason, cancelling the body, if `signal`
 * aborts first.
 */
async function readAtMost(body: ReadableStream<Uint8Array>, limit: number, signal?: AbortSignal) {
  const reader = body.getReader();
  const abort = () => void reader.cancel(signal?.reason).catch(() => {});
  signal?.addEventListener("abort", abort, { once: true });
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (size < limit) {
      signal?.throwIfAborted();
      const { done, value } = await reader.read();
      signal?.throwIfAborted();
      if (done) break;
      chunks.push(value.subarray(0, limit - size));
      size += Math.min(value.byteLength, limit - size);
    }
  } catch (error) {
    await reader.cancel(signal?.aborted ? signal.reason : error).catch(() => {});
    throw error;
  } finally {
    signal?.removeEventListener("abort", abort);
  }
  await reader.cancel().catch(() => {});
  reader.releaseLock();
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

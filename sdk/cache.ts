/**
 * The client's document cache (survey change 4): documents by id with the version they were read at, so a
 * mutation handler's `get` can be answered without a round trip.
 *
 * A cached document is never trusted. The recording executor (`sdk/recording.ts`) asserts the version of
 * every cached document a handler read, in the run's next request to the server or, at the latest, in its
 * commit; a handler that fails or returns without writing is checked the same way before its answer goes
 * to the caller. So a stale entry costs one round trip (the refused request answers the current document,
 * and the handler runs again on it), and never a commit made on old data.
 *
 * The cache is an interface so that something else can stand behind it later (a local store that a sync
 * keeps current, for local execution): it only has to answer documents with their versions.
 */
import type { Doc } from "./overlay.ts";

export interface ReadCache {
  /** The document as last seen, or `undefined`. The caller may keep and change what it is given. */
  get(table: string, id: string): Doc | undefined;
  /** Remembers `doc`, unless a newer version of it is already held. */
  put(table: string, doc: Doc): void;
  /** Forgets the document: it was deleted, or its state is no longer known. */
  delete(table: string, id: string): void;
}

/** An in-memory `ReadCache` holding at most `maxDocuments`, the least recently used going first. */
export function memoryCache(maxDocuments = 1024): ReadCache {
  const docs = new Map<string, Doc>();
  const key = (table: string, id: string) => `${table}\u0000${id}`;
  return {
    get(table, id) {
      const k = key(table, id);
      const doc = docs.get(k);
      if (!doc) return undefined;
      docs.delete(k); // re-insert: a Map iterates in insertion order, so the first key is the oldest
      docs.set(k, doc);
      return structuredClone(doc);
    },
    put(table, doc) {
      const k = key(table, doc._id);
      const held = docs.get(k);
      if (held && (held._version as number) > (doc._version as number)) return;
      docs.delete(k);
      docs.set(k, structuredClone(doc));
      if (docs.size > maxDocuments) docs.delete(docs.keys().next().value!);
    },
    delete(table, id) {
      docs.delete(key(table, id));
    },
  };
}

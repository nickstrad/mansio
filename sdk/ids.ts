/**
 * Client-generated ids (ADR 0011): document `_id`s for inserts and `requestId`s for mutations.
 *
 * Both are canonical lowercase UUIDv7 (RFC 9562 §5.7), which is what `intermediate-representation/spec/v1.md` §5.2 requires of
 * `insert.id` and which also satisfies the wire-id rule for `requestId` (§4). Layout:
 *
 * ```text
 *   48 bits unix ms | ver 7 | 12 bits counter | var 10 | 62 bits random
 * ```
 *
 * The 12-bit field is a counter within one millisecond (RFC 9562 method 1), seeded randomly in its lower
 * half, so ids from one generator sort in creation order even inside a millisecond. A generator whose
 * clock goes backwards keeps the last millisecond, so its ids never go backwards either.
 */

/** A source of UUIDv7 strings. */
export type IdSource = () => string;

const hex = (bytes: Uint8Array) => Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");

/**
 * A UUIDv7 generator. `now` and `random` are seams for deterministic tests; the defaults are
 * `Date.now` and `crypto.getRandomValues`.
 */
export function uuidv7Generator(
  now: () => number = Date.now,
  random: (bytes: Uint8Array) => void = (bytes) => void crypto.getRandomValues(bytes),
): IdSource {
  let lastMs = -1;
  let counter = 0;
  return () => {
    const bytes = new Uint8Array(16);
    random(bytes);
    let ms = Math.max(Math.floor(now()), lastMs);
    if (ms === lastMs) {
      counter++;
      if (counter > 0xfff) {
        // 4096 ids in one millisecond: borrow the next one rather than repeat an id.
        ms++;
        counter = bytes[6] & 0x07;
      }
    } else counter = ((bytes[6] & 0x07) << 8) | bytes[7]; // random start in the lower half
    lastMs = ms;
    for (let i = 5; i >= 0; i--) {
      bytes[i] = ms % 256;
      ms = Math.floor(ms / 256);
    }
    bytes[6] = 0x70 | (counter >> 8);
    bytes[7] = counter & 0xff;
    bytes[8] = 0x80 | (bytes[8] & 0x3f);
    const h = hex(bytes);
    return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
  };
}

/** The process-wide default generator. */
export const uuidv7: IdSource = uuidv7Generator();

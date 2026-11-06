/**
 * Schema fingerprints (`ir/spec/v1.md` §2): `sha256:` + lowercase hex SHA-256 of the fingerprint text.
 * One implementation for the SDK (the `schema` field of every request) and the platform (the Deno
 * writer's check, the reference executor, the vector generator). WebCrypto, so not in `core/`.
 */
import { fingerprintText } from "../core/ir/schema.ts";
import type { Schema } from "../core/schema.ts";

/** `sha256:` + hex SHA-256 over the UTF-8 bytes of `text`. */
export async function fingerprintOfText(text: string): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
  return `sha256:${Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("")}`;
}

/** The fingerprint of `schema` (its layout included). */
export function schemaFingerprint(schema: Schema): Promise<string> {
  return fingerprintOfText(fingerprintText(schema));
}

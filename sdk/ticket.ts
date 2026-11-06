/**
 * Access tickets for development and tests (IR-3): a TypeScript minter for mansio-server's
 * `internal/ticket` v1 format, so a single-node agentd in static mode (`keys_file`) can be driven by the
 * SDK without controld. Production tickets come from controld's `POST /v1/resolve`
 * (`sdk/transport.ts`'s `locatorRouter`); nothing here is needed there.
 *
 * Encoding (byte for byte `internal/ticket/ticket.go`): base64url without padding of
 *
 * ```text
 *   version (1) | len(kid) (1 byte) | kid | claims | Ed25519 signature (64 bytes)
 *   claims = str(tid) str(dbid) str(vm) uvarint(epoch) uvarint(gen) str(sub) perms (1 byte)
 *            uvarint(iat) uvarint(exp)            str(s) = uvarint(len(s)) s
 * ```
 *
 * signed over `"mansio access ticket\x00"` followed by every byte before the signature. Times are Unix
 * seconds; perms are bits (read = 1, write = 2; write does not imply read).
 */

export const PERM_READ = 1;
export const PERM_WRITE = 2;

export interface Claims {
  readonly kid: string;
  /** Tenant. */
  readonly tid: string;
  /** Database key, `tenant:app` for IR databases. */
  readonly dbid: string;
  /** Audience: the agent's `vm`. */
  readonly vm: string;
  readonly epoch: number;
  readonly gen?: number;
  /** Principal. */
  readonly sub: string;
  readonly perms: number;
  readonly iat: number;
  readonly exp: number;
}

const CONTEXT = new TextEncoder().encode("mansio access ticket\x00");
/** DER prefix of a PKCS#8 Ed25519 private key; the 32-byte seed follows. */
const PKCS8_PREFIX = [
  0x30,
  0x2e,
  0x02,
  0x01,
  0x00,
  0x30,
  0x05,
  0x06,
  0x03,
  0x2b,
  0x65,
  0x70,
  0x04,
  0x22,
  0x04,
  0x20,
];

export function base64url(bytes: Uint8Array): string {
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function fromBase64url(text: string): Uint8Array {
  const binary = atob(text.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

function uvarint(out: number[], value: number) {
  if (!Number.isSafeInteger(value) || value < 0) throw new RangeError(`uvarint: ${value}`);
  let v = value;
  while (v >= 0x80) {
    out.push((v % 0x80) | 0x80);
    v = Math.floor(v / 0x80);
  }
  out.push(v);
}

/** The bytes before the signature. */
export function encodeClaims(c: Claims): Uint8Array {
  const kid = new TextEncoder().encode(c.kid);
  if (kid.length === 0 || kid.length > 32) throw new RangeError("kid must be 1..32 bytes");
  const out: number[] = [1, kid.length, ...kid];
  const str = (s: string) => {
    const bytes = new TextEncoder().encode(s);
    uvarint(out, bytes.length);
    out.push(...bytes);
  };
  str(c.tid);
  str(c.dbid);
  str(c.vm);
  uvarint(out, c.epoch);
  uvarint(out, c.gen ?? 0);
  str(c.sub);
  out.push(c.perms);
  uvarint(out, c.iat);
  uvarint(out, c.exp);
  return new Uint8Array(out);
}

/** An issuer's signing key from its 32-byte Ed25519 seed (`frontbench keygen`'s `issuer.key`, base64url). */
export function importIssuerSeed(seed: Uint8Array | string): Promise<CryptoKey> {
  const raw = typeof seed === "string" ? fromBase64url(seed.trim()) : seed;
  if (raw.length !== 32) throw new RangeError("an Ed25519 seed is 32 bytes");
  return crypto.subtle.importKey("pkcs8", new Uint8Array([...PKCS8_PREFIX, ...raw]), "Ed25519", false, [
    "sign",
  ]);
}

/** Signs `claims` with `key`. */
export async function mintTicket(key: CryptoKey, claims: Claims): Promise<string> {
  const body = encodeClaims(claims);
  const signed = new Uint8Array([...CONTEXT, ...body]);
  const signature = new Uint8Array(await crypto.subtle.sign("Ed25519", key, signed));
  return base64url(new Uint8Array([...body, ...signature]));
}

/**
 * A fresh issuer: the signing key, its kid, and the agent's key-set file text
 * (`{"keys":[{"kid","public_key"}]}`, agentd's `keys_file`).
 */
export async function generateIssuer(kid = "k1"): Promise<{ key: CryptoKey; kid: string; keysFile: string }> {
  const pair = await crypto.subtle.generateKey("Ed25519", true, ["sign", "verify"]) as CryptoKeyPair;
  const publicKey = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  return {
    key: pair.privateKey,
    kid,
    keysFile: JSON.stringify({ keys: [{ kid, public_key: base64url(publicKey) }] }),
  };
}

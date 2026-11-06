// The Deno platform's restore. On the Go backend, `dbtool restore [-gen G] <tenant:app> <file>` (server/cmd/dbtool).
import { dirname, resolve } from "node:path";
import { objectStore, sha256 } from "../platform/object_store.ts";
import { integrityCheck, type Manifest } from "../platform/backup.ts";
const [appId, target] = Deno.args;
if (!appId || !target || !/^[A-Za-z0-9-]+$/.test(appId)) {
  throw new Error("Usage: deno task restore <appId> <NEW database path>; stop the service first");
}
const destination = resolve(target);
for (const file of [destination, `${destination}-wal`, `${destination}-shm`]) {
  try {
    await Deno.stat(file);
    throw new Error(`Refusing to overwrite ${file}`);
  } catch (e) {
    if (!(e instanceof Deno.errors.NotFound)) throw e;
  }
}
const store = await objectStore();
const manifest: Manifest = JSON.parse(new TextDecoder().decode(await store.get(`apps/${appId}/latest.json`)));
if (manifest.format !== 1 || manifest.appId !== appId || !manifest.key.startsWith(`apps/${appId}/`)) {
  throw new Error("Invalid manifest");
}
const bytes = await store.get(manifest.key);
if (bytes.length !== manifest.bytes || await sha256(bytes) !== manifest.sha256) {
  throw new Error("Backup checksum mismatch");
}
await Deno.mkdir(dirname(destination), { recursive: true });
const tmp = `${destination}.${crypto.randomUUID()}.restore`;
try {
  await Deno.writeFile(tmp, bytes, { createNew: true });
  integrityCheck(tmp);
  // Same-filesystem hard link publishes the complete file and fails if the target exists.
  await Deno.link(tmp, destination);
  console.log(JSON.stringify({ restored: destination, capturedAt: manifest.capturedAt }));
} finally {
  await Deno.remove(tmp);
}

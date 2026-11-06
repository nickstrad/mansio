/**
 * Admin CLI: deno run -A scripts/tenants.ts <command>.
 * ADMIN_TOKEN is required; BASE_URL defaults to http://127.0.0.1:8000.
 * Fresh keys appear only in create/issue-key output. No credentials are stored locally.
 */
import { Effect } from "effect";

const USAGE = `Usage: deno run -A scripts/tenants.ts <command>
  create <name> [--slug <slug>]
  list
  show <id>
  deactivate <id>
  issue-key <id> [--name <name>]
  revoke-key <key-id>
Environment: ADMIN_TOKEN (required), BASE_URL (default http://127.0.0.1:8000)`;

type Command =
  | { kind: "create"; name: string; slug: string }
  | { kind: "list" }
  | { kind: "show" | "deactivate" | "issue-key" | "revoke-key"; id: string; name?: string };

function text(value: string | undefined, label: string): string {
  // deno-lint-ignore no-control-regex -- CLI input must not contain terminal controls.
  if (!value?.trim() || /[\x00-\x1f\x7f]/.test(value)) {
    throw new Error(`${label} must be nonempty text without control characters`);
  }
  return value;
}

export function parseArgs(argv: readonly string[]): Command {
  const [kind, ...rest] = argv;
  if (kind === "list" && rest.length === 0) return { kind };
  if (!["create", "show", "deactivate", "issue-key", "revoke-key"].includes(kind ?? "")) {
    throw new Error(USAGE);
  }
  const positional: string[] = [];
  let option: string | undefined;
  const flag = kind === "create" ? "--slug" : kind === "issue-key" ? "--name" : undefined;
  for (let i = 0; i < rest.length; i++) {
    const arg = rest[i];
    if (flag && (arg === flag || arg.startsWith(`${flag}=`))) {
      if (option !== undefined) throw new Error(`${flag} may only be supplied once`);
      const value = arg === flag ? rest[++i] : arg.slice(flag.length + 1);
      if (value?.startsWith("--")) throw new Error(`${flag} needs a value`);
      option = text(value, flag);
    } else if (arg.startsWith("-")) {
      throw new Error(`Unknown option: ${arg}\n${USAGE}`);
    } else {
      positional.push(arg);
    }
  }
  if (positional.length !== 1) throw new Error(USAGE);
  const value = text(positional[0], kind === "create" ? "name" : "id");
  if (kind === "create") {
    const slug = option ?? value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
    return {
      kind,
      name: value,
      slug: text(slug, "slug (use --slug for names without ASCII letters/digits)"),
    };
  }
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value)) {
    throw new Error("id must be a canonical lowercase UUID");
  }
  return { kind: kind as "show" | "deactivate" | "issue-key" | "revoke-key", id: value, name: option };
}

type Row = Readonly<Record<string, unknown>>;

function record(value: unknown): Row {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Invalid admin API response: expected an object");
  }
  return value as Row;
}

/** Select explicit metadata columns so plaintext can never leak through a generic table renderer. */
export function table(rows: readonly Row[], columns: readonly string[]): string {
  const cells = rows.map((row) =>
    // deno-lint-ignore no-control-regex -- Keep server metadata on a single safe terminal line.
    columns.map((column) => String(row[column] ?? "").replace(/[\x00-\x1f\x7f]/g, " "))
  );
  const widths = columns.map((column, i) =>
    cells.reduce((width, row) => Math.max(width, row[i].length), column.length)
  );
  const line = (values: readonly string[]) =>
    values.map((value, i) => value.padEnd(widths[i])).join("  ").trimEnd();
  return [
    line(columns),
    line(widths.map((width) => "-".repeat(width))),
    ...cells.map(line),
    `(${rows.length} row${rows.length === 1 ? "" : "s"})`,
  ].join("\n");
}

const TENANT_COLUMNS = ["id", "name", "slug", "status", "createdAt"];
const KEY_COLUMNS = ["id", "tenantId", "name", "createdAt"];

function issued(value: unknown): string {
  const key = record(value);
  if (typeof key.plaintext !== "string" || !/^mansio_[A-Za-z0-9_-]+$/.test(key.plaintext)) {
    throw new Error("Invalid admin API response: missing issued API key");
  }
  return `${
    table([key], KEY_COLUMNS)
  }\n\nAPI key: ${key.plaintext}\nSave this API key now; it will not be shown again.`;
}

function execute(command: Command, base: URL, token: string): Effect.Effect<string, Error> {
  const request = (method: string, path: string, body?: Row) =>
    Effect.tryPromise({
      try: async (signal) => {
        const response = await fetch(new URL(path, base), {
          method,
          headers: {
            authorization: `Bearer ${token}`,
            ...(body === undefined ? {} : { "content-type": "application/json" }),
          },
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]),
          redirect: "error",
        });
        if (!response.ok) {
          await response.body?.cancel();
          // Never echo a response body or request headers: they might contain credentials.
          throw new Error(`${method} ${path}: HTTP ${response.status}`);
        }
        let decoded: unknown;
        try {
          decoded = await response.json();
        } catch {
          throw new Error("Invalid admin API response: expected JSON");
        }
        const envelope = record(decoded);
        if (!("result" in envelope)) throw new Error("Invalid admin API response: missing result");
        return envelope.result;
      },
      catch: (error) => new Error(error instanceof Error ? error.message : "Admin request failed"),
    });

  return Effect.gen(function* () {
    const root = "/admin/tenants";
    switch (command.kind) {
      case "create": {
        const result = record(yield* request("POST", root, { name: command.name, slug: command.slug }));
        return `${table([record(result.tenant)], TENANT_COLUMNS)}\n\n${issued(result.apiKey)}`;
      }
      case "list": {
        const result = yield* request("GET", root);
        if (!Array.isArray(result)) throw new Error("Invalid admin API response: expected tenant list");
        return table(result.map(record), TENANT_COLUMNS);
      }
      case "show":
        return table([record(yield* request("GET", `${root}/${command.id}`))], TENANT_COLUMNS);
      case "deactivate":
        return table([record(yield* request("DELETE", `${root}/${command.id}`))], TENANT_COLUMNS);
      case "issue-key":
        return issued(
          yield* request("POST", `${root}/${command.id}/keys`, {
            name: command.name ?? "default",
          }),
        );
      case "revoke-key": {
        // MT-05 has only tenant-scoped revocation and no key-list/owner lookup endpoint.
        // Revocation is idempotent and restricted to the URL's tenant. Trying every tenant
        // preserves the one-argument CLI without persisting secrets or reaching into Postgres.
        const result = yield* request("GET", root);
        if (!Array.isArray(result)) throw new Error("Invalid admin API response: expected tenant list");
        for (const value of result) {
          const tenant = record(value);
          const parsed = parseArgs(["show", String(tenant.id)]);
          if (parsed.kind !== "show") throw new Error("Invalid tenant id");
          yield* request("DELETE", `${root}/${parsed.id}/keys/${command.id}`);
        }
        return table([{ id: command.id, revoked: true }], ["id", "revoked"]);
      }
    }
  }).pipe(
    Effect.catchAllDefect((error) =>
      Effect.fail(new Error(error instanceof Error ? error.message : "Invalid admin API response"))
    ),
  );
}

export interface Outcome {
  readonly code: number;
  readonly stdout?: string;
  readonly stderr?: string;
}

export async function run(argv: readonly string[]): Promise<Outcome> {
  if (argv.length === 1 && (argv[0] === "--help" || argv[0] === "-h")) {
    return { code: 0, stdout: USAGE };
  }
  try {
    const command = parseArgs(argv);
    const token = text(Deno.env.get("ADMIN_TOKEN"), "ADMIN_TOKEN");
    const base = new URL(Deno.env.get("BASE_URL") ?? "http://127.0.0.1:8000");
    if (!["http:", "https:"].includes(base.protocol) || base.username || base.password) {
      throw new Error("BASE_URL must be an HTTP(S) URL without embedded credentials");
    }
    return await Effect.runPromise(
      execute(command, base, token).pipe(
        Effect.match({
          onSuccess: (stdout): Outcome => ({ code: 0, stdout }),
          onFailure: (error): Outcome => ({ code: 1, stderr: error.message }),
        }),
      ),
    );
  } catch (error) {
    return { code: 1, stderr: error instanceof Error ? error.message : "Admin CLI failed" };
  }
}

if (import.meta.main) {
  const outcome = await run(Deno.args);
  if (outcome.stdout !== undefined) console.log(outcome.stdout);
  if (outcome.stderr !== undefined) console.error(outcome.stderr);
  Deno.exit(outcome.code);
}

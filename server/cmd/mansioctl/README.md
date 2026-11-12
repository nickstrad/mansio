# mansioctl

The admin CLI for a running platform: tenants and their API keys, the database and VM lifecycle, slot-pool
limits, and one agent's runtime controls.

```sh
go build -o mansioctl ./cmd/mansioctl
export ADMIN_TOKEN=…                                     # or -token-file FILE / -secrets FILE
mansioctl -addr http://127.0.0.1:7070 tenants create "Acme Corp"
mansioctl -format table tenants list
mansioctl -addr https://controld:7080 -ca ca.pem -secrets secrets.json databases move -db "$T:app" -to vm-b
mansioctl -agent 127.0.0.1:7072 agent drain
```

## Which tool

| Tool        | For                                                                                                     |
| ----------- | ------------------------------------------------------------------------------------------------------- |
| `mansioctl` | Every admin call to a running platform: the API of agentd or controld, and each agent's admin listener. |
| `dbtool`    | The durable copies in S3, with storage credentials; works when the platform is down. Kept separate.     |
| `controld`  | The control-plane daemon, plus `keygen` and `import-mansio`, which act on its own local files.          |
| `agentd`    | The data-plane daemon.                                                                                  |

## Commands

`mansioctl [global flags] <group> <command> [flags] [args]`. Global flags go before the group; a command's own
flags may come before or after its arguments. `mansioctl -h` lists everything.

| Group       | Commands                                                                                                                                      | Listener                                                      |
| ----------- | --------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| `tenants`   | `create <name> [-slug S]`, `list`, `show <id>`, `deactivate <id>`, `issue-key <id> [-name N]`, `revoke-key <key-id>`                          | `-addr`: agentd or controld, `/admin/tenants`                 |
| `databases` | `create -tenant T -name N`, `get -db KEY`, `move -db KEY [-to VM]`, `suspend -db KEY`, `resume -db KEY [-to VM]`, `drop -db KEY`              | `-addr`: controld, `/v1/databases`                            |
| `vms`       | `list`, `attest-dead -vm VM -by WHO`, `restore -vm VM`                                                                                        | `-addr`: controld, `/v1/vms`                                  |
| `keys`      | `create -tenant T -principal P [-perms read\|write\|read\|write]`, `revoke -id KEY_ID`                                                        | `-addr`: controld, `/v1/tenants/{t}/api-keys`, `/v1/api-keys` |
| `limits`    | `set -queue QUEUE -n N`                                                                                                                       | `-addr`: controld, `/v1/limits`                               |
| `agent`     | `drain`, `backup <key>`, `passivate <key>`, `breaker <dep> trip\|reset`, `limit <queue> <n>`, `fetch-limit <n>`, `loglevel debug\|info\|warn` | `-agent`: one agentd's `admin_listen`                         |

The `databases`, `vms`, `keys`, `limits` and `agent` commands used to be `controld <command>` and `agentctl`;
their requests, validation and answers are unchanged. `controld <old command>` names its replacement and
exits 2.

`tenants create` and `tenants issue-key` print a fresh key once; `keys create` prints its key once in
controld's answer. Nothing is stored locally.

## Connecting

mansioctl reaches two kinds of listener, and each group uses one of them. Giving a group the other listener's
flags is a usage error, so an agent address never receives the admin token and an agent control never goes to
an API.

| Flag                  | Default                                                                                                           | Used by                                                                                                                                                          |
| --------------------- | ----------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-addr URL`           | `$MANSIOCTL_ADDR`, else `tenants`: `http://127.0.0.1:7070` (agentd); the controld groups: `http://127.0.0.1:7080` | `tenants`, `databases`, `vms`, `keys`, `limits`: `http` or `https`.                                                                                              |
| `-token-file FILE`    | `$ADMIN_TOKEN`                                                                                                    | The same groups: the admin token, alone in the file or as `admin_token` in a JSON secrets file (agentd's tenancy secrets, controld's `secrets.json`). Mode 0600. |
| `-secrets FILE`       |                                                                                                                   | The same as `-token-file`, the name controld's CLI used.                                                                                                         |
| `-ca FILE`            | the system roots                                                                                                  | The same groups: a PEM CA bundle for an `https` `-addr` with a private CA.                                                                                       |
| `-agent HOST:PORT`    | `$MANSIOCTL_AGENT_ADDR`, else `127.0.0.1:7072`                                                                    | `agent`: agentd's loopback admin listener, plain HTTP, no token.                                                                                                 |
| `-timeout D`          | `tenants` 30s, the controld groups 3m, `agent` 10s                                                                | All: the deadline for the whole command; `agent` also sends it as `X-Timeout-Ms`.                                                                                |
| `-format json\|table` | `json`                                                                                                            | `tenants` only.                                                                                                                                                  |

`$MANSIOCTL_AGENT_ADDR` replaces agentctl's `$AGENTCTL_ADDR`. The token is never a flag: a command line is
visible to other users of the machine (`ps`) and stays in shell history.

## Output and exit codes

`tenants`: with `-format json` stdout holds exactly one JSON document (the answer's `result`, with only the
documented fields); a note such as "Save this API key now" goes to stderr. With `-format table` the note
follows the table on stdout. Errors go to stderr as one line: the call, the HTTP status, the server's error
tag and what it means.

The controld groups print controld's JSON answer as it came, also on an error answer (with the status on
stderr). `agent` prints the agent's answer as one line; a code other than `ok` is also written to stderr.

Exit codes: 0 ok, 1 an error answer or no answer, 2 bad usage, 130 interrupted.

## Two ways to issue a tenant key

|                   | `tenants issue-key <id> [-name N]` (and `tenants create`)                    | `keys create -tenant T -principal P [-perms P]`                                                                          |
| ----------------- | ---------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Route             | `POST /admin/tenants/{id}/keys`, body `{"name"}`                             | `POST /v1/tenants/{t}/api-keys`, body `{"principal", "perms"}`                                                           |
| Served by         | agentd (tenancy `api-key` or `session`) or controld with a `tenancy` section | controld only, with or without a `tenancy` section                                                                       |
| Tenant            | must exist (a uuid made by `tenants create`) and be active                   | any id matching `[a-z0-9][a-z0-9-]{0,62}`; a new id creates a bare tenant row (no slug, no name); refused if deactivated |
| Row in `api_keys` | principal = the key's id, perms read and write, a `name`                     | the given principal (the tickets' `sub`) and perms (read, write or both), no name                                        |
| Answer            | `{"result": {id, tenantId, name, prefix, createdAt, plaintext}}`             | `{"code": "ok", id, key, tenant, principal, perms}`                                                                      |
| Revoke            | `tenants revoke-key <id>` (per tenant, idempotent)                           | `keys revoke -id ID` (by id; 404 if unknown)                                                                             |

Both keys have the form `mansio_<16 hex id>_<secret>`, live in the same `api_keys` table of the process's
Postgres schema and authenticate at controld's resolve, which reads the principal and permissions from the
row.

## Adding a command group

A group is one file and one registry entry:

1. Add `<group>.go` with a `var <group>Group = &group{…}`: a summary, the listener it calls (`apiTarget` or
   `agentTarget`), its default `-addr` and `-timeout`, whether it renders tables, and its commands. Each
   command has a usage line, a one-line summary and a `parse` function: it registers the command's flags on
   the `flag.FlagSet` it gets and returns a function that checks the positional arguments and returns the
   `action`. The action makes its calls with the `client` (`call` for the tenant admin API, `raw` for
   controld's API, `agent` for an admin listener) and returns a `view`. `tenants.go`, `controlplane.go` and
   `agent.go` are the examples.
2. Add the group to `groups` in `main.go`, and to the order in `printUsage`.
3. Add `<group>_test.go`: usage cases in a table, and each command against an `httptest` server, best the real
   handler over a fake store as `tenants_test.go` does.

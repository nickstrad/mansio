# mansioctl

The platform's admin CLI. It calls the admin API (`/admin/…`) of whichever process serves it: agentd in the
`api-key` and `session` tenancy modes, controld in a multi-node deployment.

```sh
go build -o mansioctl ./cmd/mansioctl
export ADMIN_TOKEN=…                              # or -token-file FILE
mansioctl -addr http://127.0.0.1:7070 tenants create "Acme Corp"
mansioctl -format table tenants list
```

## Commands

`mansioctl [global flags] <group> <command> [flags] [args]`. Command flags may come before or after the
arguments.

| Command                            | What it does                                                                                                                       |
| ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `tenants create <name> [-slug S]`  | An active tenant and its first API key, `default`. The slug defaults to the name in lowercase, each run of other characters a `-`. |
| `tenants list`                     | Every tenant, oldest first.                                                                                                        |
| `tenants show <id>`                | One tenant.                                                                                                                        |
| `tenants deactivate <id>`          | Deactivates the tenant and revokes its keys (idempotent).                                                                          |
| `tenants issue-key <id> [-name N]` | A new API key for an active tenant; `-name` defaults to `default`.                                                                 |
| `tenants revoke-key <key-id>`      | Revokes a key by its id (16 hex digits; a lowercase uuid for an imported key).                                                     |

A fresh key's plaintext appears only in the output of `create` and `issue-key`. Nothing is stored locally.

## Global flags

| Flag                  | Default                                         | Meaning                                                                                                                                       |
| --------------------- | ----------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `-addr URL`           | `$MANSIOCTL_ADDR`, else `http://127.0.0.1:7070` | The admin API's base URL, `http` or `https`.                                                                                                  |
| `-token-file FILE`    | `$ADMIN_TOKEN`                                  | The admin token: the token alone, or a JSON secrets file with `admin_token` (agentd's tenancy secrets, controld's `secrets.json`). Mode 0600. |
| `-ca FILE`            | the system roots                                | A PEM CA bundle for an `https` `-addr` signed by a private CA.                                                                                |
| `-timeout D`          | `30s`                                           | Deadline for the whole command.                                                                                                               |
| `-format json\|table` | `json`                                          | `json`: one JSON document on stdout. `table`: for people.                                                                                     |

The token is never a flag: a command line is visible to other users of the machine (`ps`) and stays in shell
history.

## Output and exit codes

With `-format json` stdout holds exactly one JSON document (the answer's `result`, with only the documented
fields); a note such as "Save this API key now" goes to stderr. With `-format table` the note follows the
table on stdout. Errors go to stderr as one line: the call, the HTTP status, the server's error tag and what
it means (the rest of the error body is not printed).

Exit codes: 0 ok, 1 an error answer or no answer, 2 bad usage, 130 interrupted.

## Which tool

Today `mansioctl` does platform-level admin over the admin API, `controld <command>` does the control plane's
database and VM lifecycle, and `agentctl` drives one agent's loopback admin listener. Consolidating them under
`mansioctl` as further groups is a possible later step.

## Adding a command group

A group is one file and one registry entry:

1. Add `<group>.go` with a `var <group>Group = &group{summary: …, commands: map[string]*command{…}}`. Each
   command has a usage line, a one-line summary and a `parse` function: it registers the command's flags on
   the `flag.FlagSet` it gets and returns a function that checks the positional arguments and returns the
   `action`. The action makes its calls with `client.call` and returns a `view` (the JSON document, the table,
   an optional note). `tenants.go` is the example.
2. Add the group to `groups` in `main.go`.
3. Add `<group>_test.go`: usage cases in a table, and each command against an `httptest` server, best the real
   handler over a fake store as `tenants_test.go` does.

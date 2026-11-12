# agentd runtime admin controls and mansioctl agent

agentd's loopback admin listener (breaker trip/reset, fetch limit, drain, passivate, log level) and the
`mansioctl agent` CLI that drives it.

Rules it follows: an operator can trip and reset breakers; control requests never queue behind queries; every
control has a deadline; a CLI comes first; components are controlled at run time without a deploy. Code:
`cmd/agentd/admin.go`, `cmd/mansioctl`, `internal/breaker` (`Trip`, `Reset`), `internal/vactor`
(`SetFetchLimit`).

## Listener

`admin_listen` (optional, e.g. `127.0.0.1:7072`): a third listener, separate from the query and lifecycle
listeners, with the lifecycle listener's server-wide timeouts (`healthServer`). **It must be a loopback
address (localhost or a loopback IP), with or without `-dev`**: binding is the only access control (anyone who
can reach a loopback port has a shell on the VM and could signal the process anyway). `:7072` or a routable IP
is a config error. The addr-file gets the admin address on a third line (its second line is now always
written: the query address when the lifecycle endpoints share the query listener).

## Endpoints

All `POST`, JSON answers, each under `lifecycle_timeout` (an `X-Timeout-Ms` header may shorten it), at most 4
in flight (more → `503 unavailable`, never queued), each action logged once at WARN so it survives
`log_level: warn`.

| Endpoint                     | Does                                                                                                                                                                        | ok answer                                        |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| `/admin/breaker/{dep}:trip`  | `Breaker.Trip`: forced open until reset                                                                                                                                     | `{"code":"ok","dep","state","forced","changed"}` |
| `/admin/breaker/{dep}:reset` | `Breaker.Reset`: closed, fresh window                                                                                                                                       | same                                             |
| `/admin/fetch:limit?n=N`     | `vactor.Host.SetFetchLimit(N)`, N ≥ 1                                                                                                                                       | `{"code":"ok","fetch_limit","previous"}`         |
| `/admin/drain`               | `front.Server.Drain()`: new queries shed (`shed`, 503); `/readyz` and `/healthz` say `draining`. One way: the process keeps running; its shutdown drain later finds it done | `{"code":"ok","in_flight"}`                      |
| `/admin/passivate?key=K`     | `vactor.Host.Passivate`: write back, drop the warm copy (no-op if not warm)                                                                                                 | `{"code":"ok","key"}`                            |
| `/admin/loglevel?level=L`    | `slog.LevelVar` (debug, info, warn)                                                                                                                                         | `{"code":"ok","level","previous"}`               |

Errors: `{"code","message"[,"log_id"]}` — `bad_request` 400 (bad n/level/key/action), `not_found` 404 (unknown
breaker, unknown path, wrong method, or a durable-only control in static mode), `unavailable` 503 +
Retry-After, `timeout` 504, `internal` 500 + log ID. Event codes: `ADMIN.BREAKER_TRIP`, `ADMIN.BREAKER_RESET`,
`ADMIN.FETCH_LIMIT`, `ADMIN.DRAIN`, `ADMIN.PASSIVATE`, `ADMIN.LOGLEVEL`, `ADMIN.REFUSED`, `ADMIN.FAILED`; the
breaker's own transition log (`S3.BREAKER_OPEN`, …) now carries `"manual": true` for Trip/Reset.

Breakers come from `backend.breakers`, a `map[string]*breaker.Breaker` (durable mode: `{"s3": …}`; the
placement breaker joins with one map entry). Static mode has no breakers, fetch limit or passivate.

## Semantics added below

- `breaker.Trip(now) bool`: forced open; `Allow` refuses with the cool-down as the retry hint, past the
  cool-down, with no half-open probe; `State` = open; `Forced()` = true. `Reset(now) bool`: closed from any
  state with a fresh window. Both are transitions (`Transitions()` counts them; permits issued before,
  including a half-open probe, are ignored) reported through `OnChange` with `Transition.Manual`; a repeat
  returns false and reports nothing. Invariant gains `forced ⇒ Open`; the model fuzz covers trip/reset.
- `vactor.Host.SetFetchLimit(n)`, `FetchLimit()`: fetches running when the limit drops finish; no new one
  starts until fewer than n run; raising it grants queued activations at once. Byte and queue bounds
  unchanged.

## mansioctl agent

```
mansioctl [-agent 127.0.0.1:7072] [-timeout 10s] agent breaker <dep> trip|reset
mansioctl [flags] agent fetch-limit <n> | drain | passivate <key> | loglevel debug|info|warn
```

`-addr` defaults to `$AGENTCTL_ADDR`, else `127.0.0.1:7072`; `-timeout` is also sent as `X-Timeout-Ms`. Prints
the JSON answer on stdout. Exit 0 ok, 1 any other answer / no answer / non-JSON / timeout, 2 usage (checked
before sending), 130 interrupted.

## Gate results (2026-11-05, M5 Max, in-process s3test fake; `go test -run TestAdmin ./cmd/agentd`)

| Gate                          | Result                                                                                                                                                                                                                                                   |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Trip the S3 breaker via admin | cold key → `503 unavailable`, Retry-After ≥ 1, **0 S3 requests for that key** (counted on the fake), three times across the 3 s cool-down; warm key reads and writes ok; `mansio_breaker_state{dep="s3",state="open"}` 1; reset → the cold key activates |
| Lower fetch:limit at run time | limit 1, three cold keys at once, GETs dribbled at 16 KiB/s: peak 1 fetch in flight, 2 queued, all ok; raised to 3 (after passivating them): peak 3 in flight                                                                                            |
| Other controls                | passivate wrote back (manifest gen ≥ 1) and the key re-warmed with its rows; drain → queries `shed`, `/readyz` 503 draining, then exit 0 on stop; every error code                                                                                       |

## Decisions

- Admin binds loopback only, always, no token: "only the control plane may use runtime controls" is met by the
  bind; a remote operator goes through a shell on the VM. If controld ever needs these remotely, add the
  control token check (the secrets file already holds it) before widening the bind.
- Admin drain is one way (no undrain): a drained agent is meant to be restarted or replaced.
- Admin actions log at WARN: operator actions change behavior and must survive `log_level: warn`.

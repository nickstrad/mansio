# 2026-11-04 — sqlbench: what the testing approach found

Author: sqlbench agent. Code: `internal/{actor,bench,load,stats,schema,workload,retry,outcome}`, `cmd/bench/sqlbench`.

- **Fuzz seeds share names, so temp paths contain `#`.** Subtests named `seed#0` get temp dirs such as
  `.../FuzzXseed#0123/001`. Building a SQLite DSN as `"file:"+path+"?..."` cuts the path at `#`, so every seed
  opened one shared file (`$TMPDIR/FuzzXseed`) and saw the others' rows. The model fuzzer caught it on its
  first seed run. Fix: percent-escape the path (`actor.dsn`). Regression:
  `TestPathsWithURICharactersOpenTheirOwnFile`. Lesson: give fuzz seeds a real database each, and make the model
  check the whole state after each step, not just the keys the step touched.
- **Property tests found an invalid option at its boundary.** `FuzzGenerate` hit `min_columns: 0`. A root table
  with no columns gives `INSERT INTO t () VALUES ()`, which SQLite rejects. The spec table now has the on point
  (1, valid) and the off point (0, invalid).
- **An invariant over the whole run, checked at the end.** Every request ends in exactly one final attempt:
  the count of `final=true` rows equals the first attempts, including gen_full rows and retries lost to a full
  queue. `TestOverloadRecordsEveryRequestOnce` checks it under overload with a retry budget. Also asserted in
  `bench.run`: records written + dropped == records added.
- **Pure pacing makes the rate testable.** `load.Pacer.Due(now)` takes the clock as an argument.
  `FuzzPacerSendsTheIntegratedRate` checks that any tick pattern sends the integrated rate, within the error
  bound of the trapezoid rule. 1.3M cases in 10 s.
- **Speed differs a lot between targets.** Pure targets run at 0.1–1M execs/s. The actor model target opens a real
  database per input (about 15/s), so it needs minutes, under the bench lock: `make sqlbench-fuzz`.

# docs/knowledge/ — rules for agents

Several agents and sessions work in mansio, and context gets cleared. This folder keeps what they learned, so
the next agent does not have to work it out again. The maintenance convention in [`index.md`](index.md) still
applies; this file adds topic folders and thread capture.

## Two kinds of entry

- **Flat pages** (`decisions.md`, `gotchas.md`, `glossary.md`): short entries that cut across topics. Keep
  using them.
- **Topic folders** (`<topic>/`): longer explanations of one part of the system, such as the compiler, the
  query path, the reader process, or tenancy. Use these for anything that took a whole thread to explain.

## Thread capture

Write an entry when a session:

- answers a "how does X work" or "walk me through X" question,
- traces a request, query, or write across modules or processes,
- debugs something and finds out why it behaves the way it does, or
- corrects something an earlier entry or answer got wrong.

Write it in the same session, before the final reply. Don't leave it for later. Skip trivial lookups (one
file, one fact) and anything a plan, ADR, or `docs/architecture/` page already covers. Link to those instead.

## Topic folder layout

```
docs/knowledge/<topic>/
  README.md                 what the topic covers, newest entry first, one line each
  YYYY-MM-DD-<slug>.md      one entry per thread or finding
```

1. **Topic names** are short kebab-case nouns for a part of the system: `compiler`, `query-path`,
   `reader-process`, `tenancy`. Before you make a new folder, check `index.md`. Add to an existing topic when
   one fits.
2. **Each entry** opens with: date, the question it answers, and the commit it was read at
   (`git rev-parse --short HEAD`). Then the explanation, step by step, with `file:line` references, real
   examples, and diagrams if they help.
3. **Evidence.** Say how each claim was checked: "ran `compileSelect` on this plan, output below" or "read
   `platform/http/handlers.ts:175`". Mark anything inferred and not checked as _unverified_.
4. **Save the scripts.** Code you run to check a claim (a probe, a trace, a one-off query) goes in
   `work-scripts/` and gets a row in `work-scripts/INDEX.md` (rules: `work-scripts/README.md`). Never leave it
   in `/tmp` or a scratchpad. In the entry, give the exact command that reproduces the output you quote, and
   list the script in the topic's `README.md`. Extend an existing probe before you write a new one.
5. **Corrections.** If an answer in the thread was wrong and later fixed, the entry records the fixed version.
   If it contradicts an older entry, add a dated `Superseded by …` line to the old one. Don't delete it.
6. **Index.** Add one line per topic folder to `index.md` under _Topics_. Add an indented line for an entry
   only if it is a key one. Keep the index short.
7. **Ownership.** Anyone may add an entry to any topic. Don't rewrite someone else's entry. Add a dated
   correction entry and link it from theirs.
8. **Branch rules.** Knowledge entries are docs-only commits (`knowledge: <summary>`). A docs-only session may
   write them. They never edit code.
9. Run `deno fmt --check docs/knowledge/` after edits.

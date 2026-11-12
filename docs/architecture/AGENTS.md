# Agent instructions for C4 diagram work in `mansio`

Standing conventions for every architecture diagram in this repo. Read this before creating or editing
anything under `docs/architecture/`. For the orchestration protocol that says _when_ diagrams get updated, see
the repo-root [`AGENTS.md`](../../AGENTS.md) section **"Diagram maintenance (C4 architecture docs)"**.

## 1. The model: Simon Brown's C4

All architecture diagrams in this repo follow the [C4 model](https://c4model.com/) by Simon Brown, across its
four levels:

| Level | Name               | Scope                                                                                                           | Audience                              |
| ----- | ------------------ | --------------------------------------------------------------------------------------------------------------- | ------------------------------------- |
| 1     | **System Context** | the platform as one box, its people, and the external systems it talks to                                       | anyone                                |
| 2     | **Container**      | the separately running / separately deployed things inside the platform: processes, files, stores               | developers and operators              |
| 3     | **Component**      | the major building blocks inside one container — for this repo, the Effect services and layers inside a process | developers working in that container  |
| 4     | **Code**           | class/type structure inside one component; drawn only where it earns its keep                                   | the developer changing that component |

Rules that follow from the model:

- **One level per diagram.** Do not mix containers and components in the same picture. If a diagram needs a
  container to explain a component, draw the container as a plain box on the boundary and leave its internals
  out.
- **Boundaries are explicit.** A system boundary or container boundary is drawn, not implied.
- **Every element is labelled with its type** — `[Person]`, `[External system]`, `[Deno process]`,
  `[SQLite 3, WAL mode]`, `[filesystem]` — and every relationship is labelled with what actually travels over
  it (protocol, payload, direction).
- **Level 4 is optional.** Only draw a Code-level diagram when the type structure is genuinely hard to read
  from the source. Prefer no diagram over a diagram that restates the file.
- **Prefer two small diagrams over one cluttered one.** Splitting Level 2 into a request path and an
  observability path (as this repo does) is the intended use of the model, not a workaround.

## 2. Every diagram exists as two files

Each diagram is published in **two formats**, and both are required:

| File                 | Format                                                         | Read by                                                     |
| -------------------- | -------------------------------------------------------------- | ----------------------------------------------------------- |
| `<name>.github.md`   | [Mermaid](https://mermaid.js.org/) in a `` ```mermaid `` block | GitHub's Markdown viewer, which renders Mermaid natively    |
| `<name>.terminal.md` | UTF-8 box-drawing / ASCII art inside a `` ```text `` block     | a terminal, via `glow docs/architecture/<name>.terminal.md` |

**They are two renderings of one diagram, not two diagrams.** The same elements, the same boundaries, the same
relationships, the same planned/built markers, in the same reading order. A change to one is incomplete until
the other matches. Where the terminal rendering cannot draw an edge without clutter, that edge still has to
appear in the terminal file's **numbered relationship key** — the key and the Mermaid graph must list exactly
the same relationships.

### Naming

```
c4-<level>-<slug>.github.md
c4-<level>-<slug>.terminal.md
```

`<level>` is one of `context`, `container`, `component`, `code`. `<slug>` names the subject — the system for
Level 1, the grouping for Level 2, the container whose insides are shown for Level 3. Examples:

```
c4-context-document-db.github.md      c4-context-document-db.terminal.md
c4-container-platform.github.md       c4-container-platform.terminal.md
c4-component-server.github.md        c4-component-server.terminal.md
```

Both files for one diagram share the stem exactly, so `ls docs/architecture/` pairs them up.

## 3. Marking what is not built yet

The diagrams draw the **current** tree, so that a reader can trust a solid box. This repo is mid-way through
the Effect-TS port in
[`docs/plans/completed/effect-port-and-observability.md`](../plans/completed/effect-port-and-observability.md);
a target view is allowed only as an explicitly labelled element or edge inside a current diagram. Anything
that does not exist on `main` must be unmistakable:

- The literal word **`(planned)`** or **`[planned]`** in the element's label, with the plan item reference
  (`(item 23)`) if the item has not landed on `main`.
- In the Mermaid format: the `planned` CSS class (dashed stroke), declared in every diagram as
  `classDef planned stroke-dasharray: 5 3`, and a dashed edge (`-.->`) for a planned relationship.
- In the terminal format: dashed box borders (`┄` and `┆`) instead of solid (`─` and `│`).

Belt and braces is deliberate. A reader with a font that renders `┄` as `─`, or a Mermaid viewer that drops
the `classDef`, still sees the word. **Never rely on styling alone to mark something as planned.**

Separate **available** from **connected**. A library that exists but is mounted by no process (for example
`startRetentionSweeper`) is not drawn as a running component. A solid edge means the relationship is wired in
a process that runs, and it must be checked against the code that wires it, not against a unit test of the
library. Do not present a reflection snapshot or a past test run as the current state; record the date and
commit of any evidence.

When a plan item lands, the element's borders go solid, the `planned` class is dropped, and the `(item NN)`
reference stays as provenance.

## 4. Diagrams live with the code and must track it

Diagrams are part of the source tree, not a one-off deliverable. The repo-root `AGENTS.md` requires a
follow-up docs agent after every landed plan item; it reads the landed diff, decides which diagrams it
affects, updates **both formats**, and commits. Specifically:

- A new process, file, store, or external dependency is a **Level 2** change.
- A new Effect service, layer, or a changed dependency between services is a **Level 3** change.
- A new kind of caller, or a new external system, is a **Level 1** change.
- A reviewer rejects a "diagrams are current" claim that was not re-verified against the diff.
- If an item touches no diagram-relevant surface, say so in one line and move on.

Do not leave a diagram describing code that no longer exists. A wrong diagram is worse than no diagram,
because it is believed.

## 5. How the terminal format is produced

> **Status note (2026-10-10).** A `draw-visual` skill **is** installed on this host, at
> `~/.claude/skills/draw-visual` (with a `visual-drawer` agent). It renders Mermaid sources to Unicode text
> with `mermaid-ascii`, which is exactly the right tool for flow, topology and sequence diagrams — **prefer it
> for those.** It is _not_ used for the C4 diagrams here, for two reasons verified on this droplet on
> 2026-10-10:
>
> 1. **It cannot draw C4 boundaries.** `subgraph` is a documented failure mode of the renderer
>    (`reference.md`: "misplaces nodes and detaches edges"). A test render of a four-node graph with one
>    `subgraph` put the parent node outside its own boundary and ran the boundary's bottom border straight
>    through an unrelated node. Boundaries are not decoration in C4 — Levels 1-3 are defined by them.
> 2. **Its node budget is too small.** The renderer is documented as good to about eight to ten nodes before
>    output exceeds 150 columns; the container and component diagrams here carry nine to ten elements plus a
>    boundary.
>
> So the terminal format for C4 diagrams is box-drawing generated by the small drawer in `diagrams/` (below).
> **If a C4-capable terminal renderer becomes available — a draw-visual version that handles `subgraph`, or a
> dedicated C4 text renderer — prefer it over this drawer and update this section.**

The terminal art is **generated, not hand-edited**. Sources live in `docs/architecture/diagrams/`, beside the
documents they feed (the same convention `draw-visual` uses for its `.mmd` sources):

```
docs/architecture/diagrams/
  canvas.py                        # the drawer: boxes, connectors, boundary frames, width asserts
  c4-context-document-db.py        # one source per terminal diagram
  c4-container-platform.py
  c4-container-observability.py
  c4-component-server.py
  render.py                        # rewrites the embeds in ../*.terminal.md
```

Each `*.terminal.md` marks its embed so the regenerator can find it:

````markdown
<!-- c4-render: diagrams/c4-context-document-db.py -->

```text
(generated; do not hand-edit)
```
````

From the repo root:

```sh
python3 docs/architecture/diagrams/render.py           # regenerate every embed
python3 docs/architecture/diagrams/render.py --check   # non-zero exit if any embed is stale
```

`--check` proves the embed matches its source. It does **not** prove either one matches the code; that is
still a reading job for the author and the reviewer.

These are Python, not Deno, and they are diagram _sources_ rather than investigation scripts, so they stay
here rather than under `work-scripts/` — `work-scripts/README.md` governs reusable work scripts, not document
assets.

### Keeping it readable in a terminal

- **Hard limit 72 columns**, and `canvas.py` asserts it. `glow` wraps at 80 by default and adds a left margin
  to code blocks; anything wider wraps and the art falls apart.
- Vertical flow top-to-bottom. Two columns at most; route an edge that skips a tier down a dedicated channel
  rather than drawing a diagonal.
- Short edge labels — a bracketed number `(3)` into a relationship key underneath. Terminals have no room for
  a sentence on an arrow.
- Boundary frames use `╭ ╌ ╎ ╰` so a boundary never reads as a container box.

## 6. Mermaid conventions for the GitHub format

- Use `flowchart TB` with `subgraph` for boundaries. **Do not use Mermaid's native `C4Context` / `C4Container`
  / `C4Component` syntax** — it is still experimental and GitHub's renderer support for it is unreliable.
  Plain `flowchart` with subgraphs renders everywhere.
- Declare the same three classes in every diagram, so the whole set looks like one set:
  ```
  classDef planned stroke-dasharray: 5 3
  classDef person fill:#fff,stroke-width:2px
  classDef external fill:#f4f4f4
  ```
- Put each element's type in its label on its own line (`<br/>[Deno process]`), and the file or path it
  corresponds to underneath. A reader should be able to go from a box to a file.
- Label every edge with the protocol and payload, and number it to match the terminal file's relationship key.
- Keep the prose around the diagram identical in both formats: the title, the "what this shows" paragraph, the
  relationship key, and the notes. Only the picture differs.

## 7. Checklist before committing a diagram change

1. Both `<name>.github.md` and `<name>.terminal.md` updated, with the same elements and edges.
2. `python3 docs/architecture/diagrams/render.py --check` exits 0.
3. Every element that is not in `main` yet is marked `(planned)` **and** dashed.
4. Every element names the file or path it corresponds to, and every path named actually exists (or is marked
   planned).
5. `docs/architecture/README.md` lists any diagram you added, with links to both formats.
6. The terminal format read under `glow` at an 80-column width without wrapping.

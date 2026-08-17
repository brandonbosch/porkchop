# porkchop — a TUI-first reviewer for LLM code changes, built on meat

*A fork of [boldsoftware/meat](https://github.com/boldsoftware/meat): the same trustworthy abridging core, plated the way a reviewer actually wants to eat it.*

## The real goal

Porkchop is a tool for **reviewing LLM-generated code changes** — quickly, and with justified trust. Models write a lot of code now; a human still has to review it in critical systems, but shouldn't have to read the mechanical noise. Meat already solved the hard half of that: it turns a raw diff into a **reading diff** — the same change with the noise subtractively elided — and it does so with an integrity guarantee (the model authors an *edit plan*, meat applies it to the immutable original, so the model never writes the displayed diff wholesale). Meat's weakness is the last mile: it hands the reading diff to `less`.

Porkchop keeps meat's core untouched and gives the reading diff the presentation a reviewer deserves — a Bubble Tea / Lip Gloss TUI modeled on the best diff UI I know (the JetBrains class of IDE): clean classification, intra-line highlighting, keyboard stepping, and — critically — **cheap verification of what meat elided**.

**The artifact is the reading diff, not a comparison against the original.** An earlier HTML prototype (Fable-generated) showed the *original diff* beside the *live meat output* — that was built to **evaluate meat itself**, and it did that job well. But the product is not a meat-evaluation tool; it's a review tool. So the pre-meat original diff does **not** get a standing pane. It doesn't disappear either: it becomes **on-demand verification** — expand a fold (`...`) in place to see exactly what was cut, or open the audit view to skim the whole discard pile. For reviewing LLM changes you can't trust an abridgement you can't cheaply check, so "show me what it hid" is a first-class gesture, not a permanent split.

Design principle, inherited and extended: **the model supplies judgment, the machine supplies integrity, the reviewer supplies attention — and the UI's whole job is to spend that attention well.**

## Hard constraint: CUI → AWS Bedrock

Most code reviewed with this tool is **CUI (Controlled Unclassified Information)**, so the diffs porkchop feeds a model are CUI. Public OpenAI/Anthropic APIs are therefore off limits: **live diff review must go through an AWS Bedrock endpoint** in a CUI-authorized account/region. Notes:

- The meat agent's read-only tools (`read_file`/`grep` over the repo root) also send **surrounding source** to the model, not just the diff — so the *entire* backend must be the compliant enclave; there is no "only the diff leaves" carve-out.
- The `~/.meat` cache (and porkchop's future `.porkchop.json` sidecar) store the reading diff — CUI — at rest on local disk. Fine in an authorized environment, but it's a real artifact to account for.
- This gates every *live* run. It does **not** gate UI development: Phases 1–3 build and test entirely offline against `meat/testdata` fixtures (see Execution). Bedrock is a parallel prerequisite for pointing porkchop at real repos, not a blocker for the UI.

Implementation seam: meat's `Model` interface (`meat/model.go`) is provider-agnostic. A Bedrock backend is an **additive** `Model` implementation in porkchop-land (`internal/bedrock`), constructed by `cmd/porkchop` instead of `meat.NewModelFromEnv` — **no change to meat core**. Cleanest path: AWS SDK for Go v2 `bedrockruntime` **Converse** API (normalizes tool use across models), translating meat's `Message`/`Block`/`Tool`. `ANTHROPIC_BASE_URL` alone won't do — Bedrock needs SigV4, not the `x-api-key` header.

## Fork strategy

Treat `meat/` (the core package) as **vendored upstream code you never modify** except through upstream merges — the upstream is young and moving fast, and we want its rubric/compiler improvements for free. License is Apache 2.0: upstream copyright preserved, `NOTICE` states the modification, all additions Apache 2.0 too.

**Decided and done (Phase 0):** the Go module was renamed `meat.dev` → `github.com/brandonbosch/porkchop`. This was cheaper than the original plan feared: `meat/` has **zero** `meat.dev/`-prefixed imports, so the core tree stays byte-identical to upstream under the rename — `git merge upstream/main` never conflicts on it. Only `cmd/meat`'s import lines moved (and `cmd/meat` is a secondary artifact, kept only for behavior comparison). `upstream` remote points at boldsoftware/meat and is fetched.

Repo layout:

```
porkchop/
  meat/                 # upstream core, pristine (LLM planner + edit-plan compiler)
  cmd/meat/             # upstream CLI (import paths rewritten; kept for behavior diffing)
  cmd/porkchop/         # the porkchop binary + subcommands
  internal/ui/          # Bubble Tea models, Lip Gloss styles, keymaps  (owns ALL styling)
  internal/diffview/    # pure: reading diff -> semantic row model (no styling, no terminal)
  internal/store/       # cache: meat's ~/.meat results + porkchop sidecar metadata
  internal/gitx/        # git log/show/range plumbing (copied from cmd/meat, kept pristine there)
  internal/bedrock/     # (later) CUI-compliant meat.Model over AWS Bedrock Converse
```

## What porkchop adds (and what it deliberately doesn't)

Porkchop is a renderer and workflow around `meat.Abridge`. It does **not** touch the rubric, the edit-plan compiler, move detection, import stripping, or chunking. Every pixel it draws comes from the original diff plus meat's subtractive plan.

**The seam is two strings.** Everything porkchop renders is a pure function of the **raw diff** and **`meat.Result.SmartDiff`** (plus the one-line `.Summary`). `meat.Result` carries nothing else of substance. This collapses the whole design: side-by-side alignment, intra-line highlighting, fold expansion, audit view, and the header manifest are all `f(rawDiff, smartDiff)` — no git, no LLM, no cache, no terminal required to compute any of them.

**One core-adjacent need:** meat discards the model's edit plan after applying it, keeping only the final `SmartDiff`. Porkchop wants elision→original-line ranges for fold expansion and the audit view. Get it by recomputing — align `SmartDiff` back onto the raw diff (approach **(a)**: pure, no core change). A working reference for this alignment already exists in the core: `meat/elision.go`'s `retainedDiffStats` (exact-match index + compiled elision-projection regex). Later, optionally, offer upstream a three-line addition exposing the applied plan (approach **(b)**) and drop the recompute.

**Done (Phase 3):** approach (a) is implemented as `diffview.Align`, and it cross-checks exactly against `meat.ElisionLine`'s own counts on all three goldens. Approach (b) is now much less attractive: the recompute turned out to yield *more* than the plan would (see the fold-row finding below), so there is little left to ask upstream for.

## TUI design

Primary artifact: **the reading diff**, presented as well as JetBrains presents a diff. Presentation grows in a ladder:

**Header.** Meat's one-line summary plus a locally computed **stat manifest** — the tiles from the prototype: changed rows shown (e.g. `83/109`, 76% retained), changed rows elided (`26`), physical rows, bytes reduction. All computed locally from the two diff strings (extends `meat.ElisionLine`).

**Body — Phase 1 (unified).** The reading diff in one column: green added / red removed / **amber folded** classification, viewport scroll, quit. One pane, so it works at any width including narrow CUI sessions.

**Body — Phase 2 (JetBrains split).** Old │ new split of the reading diff with **intra-line token highlighting** (bright background on the differing tokens, standard red/green elsewhere). A `u` toggle swaps unified ↔ split for small terminals. Navigation: hunk stepping, file stepping, a file strip / tree, search.

**Trust — Phase 3 (the point of the tool).** `e` expands the elision under the cursor **in place** (dimmed, behind a `│` rail — "the model called this noise"), `e` again re-collapses; `E` expands or collapses all; `n`/`p` step between elisions. `a` flips to an **audit view** showing *only* what was elided, grouped by file. This is where the original content surfaces — on demand, in flow. No web viewer, pager, or meat itself offers this.

**Load-bearing finding (2026-08-17): meat's `...` fold rows are not where the elision lives.** Measured across all three goldens, meat emits 1–2 fold rows per commit but leaves 14–27 distinct gaps that hide changed lines (62–90 changed lines total); the largest single unmarked gap in the pytest golden hides **25 changed lines**. Keying expansion to fold rows — as this plan originally specified — would therefore have exposed roughly a twentieth of the hidden content, which is not a basis for trust.

So the unit of expansion is the **alignment gap**, not the fold row, and porkchop **synthesizes** a marker wherever meat dropped changed lines silently. A fold row, where meat did emit one, is recorded as an attribute of the gap it marks and its marker stands in for it (the marker says "12 changed lines hidden", which strictly dominates what `...` conveys).

Gaps that hid *only* context get no marker and are omitted from the audit listing, reported as counts instead. That follows from meat being subtractive: the reading diff's lines are a subsequence of the original's, so no line can be reclassified, and a context line is by definition identical in the old and new versions. A gap with zero `+`/`-` lines therefore **cannot conceal any part of the change** — it is exactly the comprehension padding meat exists to drop.

**Commit picker (Phase 2/4).** A `git log` list with a per-commit status glyph: processed (cache hit, opens instantly), processing (spinner fed by meat's `Progress`), unprocessed. Accepts everything meat's CLI accepts and jumps straight to review.

Keybindings, JetBrains-flavored: `j/k` scroll, `g/G` top/bottom, `n/p` elision (Phase 3; will extend to hunk stepping in Phase 2), `]/[` file, `tab` focus, `e`/`E` expand, `a` audit, `/` search, `u` unified toggle, `q` quit. Mouse wheel works for free in Bubble Tea.

**Pinned for later — change-overview commentary.** An area of LLM-generated prose about the change: concise per-area descriptions and a breakdown of the overall change (the useful spirit of the prototype's hand-written "Preserved well / Budget critique" sidebar, but *generated*). This is a second, cheaper LLM pass — not free from `meat.Result`, and its exact content will evolve. We'll already be running an LLM (via Bedrock), so it's natural to add. **Deliberately deferred**; parked here so it isn't lost.

## Rendering pipeline (internal/diffview)

`diffview` is a **pure transform**: it takes the diff strings and emits a **semantic row model** — row kind (`meta`/`hunk`/`add`/`del`/`fold`/`context`), cell content, and (later) intra-line span roles and fold→raw-range mappings. It does **no** Lip Gloss styling and needs no terminal; **all styling lives in `internal/ui`**. This keeps the hardest logic unit-testable against fixtures with a binary pass/fail signal, and confines every visual/taste decision to one place. (This overrides the earlier "style with Lip Gloss in diffview" note.)

- Parse **leniently**: meat documents that `@@` hunk counts go stale after line removal — never trust the counts, read until the next header. Classification ports from the HTML prototype.
- Phase 1 is unified: parse → classify → rows. No alignment needed.
- Phase 2 adds split alignment: within a hunk, context rows align 1:1; paired `-`/`+` runs align row-by-row with blank filler for unequal lengths; **fold rows are full-width rows spanning both columns**. The same pairing feeds intra-line highlighting (tokenize words+punctuation, token-level Myers/LCS, emit styled spans; test against delta's fixtures for sanity).

Note meat core has **zero dependencies** — keep it that way. Bubble Tea / Lip Gloss / bubbles deps belong to `cmd/porkchop` and `internal/`; `internal/diffview` stays pure stdlib; the `meat/` tree stays mergeable and upstreamable.

**TUI stack: Charm v2** (`charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`). Phase 1 built on v1 because v2 was still pre-release; it went GA before Phase 2, and Phase 2 is the phase that cashes it — `bubbles/v2`'s viewport supplies match highlighting with next/prev stepping (`SetHighlights`/`HighlightNext`, i.e. the `/` search feature), a `LeftGutterFunc` column that survives horizontal scrolling (line numbers for the split view), and `SoftWrap`. Consequences to know: `View()` returns a `tea.View` and declares alt-screen/mouse itself (the `WithAltScreen`/`WithMouseCellMotion` program options are gone), key messages are `tea.KeyPressMsg`, and Lip Gloss colors are `image/color.Color` with no `AdaptiveColor` — `internal/ui` resolves a light/dark pair explicitly via `lipgloss.LightDark`, seeded dark and corrected from `tea.BackgroundColorMsg`, which keeps offline golden rendering deterministic. Requires Go ≥ 1.25.

## Workflow integration

`porkchop process [rev|range]` runs meat headlessly and fills the cache — the post-commit-hook / agent-devtool entry point. Cache layout: reuse meat's `~/.meat` content-addressed store untouched (same key = free interop with plain meat), adding `<key>.porkchop.json` sidecars for plan/audit metadata. `porkchop hook install` writes the post-commit hook. Later: a small `fsnotify` watcher that pre-processes new commits in open repos; per-file "viewed" markers (JetBrains' checkbox).

## Execution / how the work divides

The two-string seam sets the decomposition. The dependency graph is three **leaves** — `gitx`, `store`, `diffview` (each depends only on meat + stdlib) — and two **consumers** — `ui` → `diffview`, and `cmd/porkchop` → all. Slice work by package seam: each unit's working set is its own package + the two-string contract + `meat.go`'s ~100-line public API. Nothing ever needs to load meat's ~12k-line internals.

Recurring pattern across phases: **hard/pure/testable logic → `diffview` (delegate to a subagent with a fixture-test spec); visual/interactive/judgment → `ui` (hand-build); plumbing → `gitx`/`store`/`cmd` (quick).** The archetypal delegation is Phase 2's split-alignment + intra-line highlighting: pure, hard, and fixture-testable against `meat/testdata` (`<name>.diff` = raw input, `<name>.golden.diff` = reading diff, `<name>.plan.json` = the compiled plan — which also prototypes the Phase-3 sidecar).

## Phases

**Phase 0 — fork mechanics. ✅ DONE (2026-08-04).** Module renamed; `meat/` pristine; `upstream` remote added + fetched. `internal/gitx` and `internal/store` are self-contained copies of `cmd/meat`'s git + cache plumbing (so `cmd/meat` stays byte-pristine); `store.Key` is identical to meat's, so cache interop is guaranteed. `cmd/porkchop` skeleton accepts meat's args, shares the cache, renders plain text. `NOTICE` records the Apache-2.0 lineage. `internal/store` unit-tested. Build/vet/tests green. On branch `phase-0-fork`.

**Phase 1 — unified reading-diff TUI.** `internal/diffview` parses `SmartDiff` into classified semantic rows; `internal/ui` renders one column (green/red/amber), the header (summary + stat manifest), viewport scroll (`j/k`, mouse), `q`. Reads cache or computes (compute needs Bedrock/creds; **develop and test against `meat/testdata` goldens, no LLM**). *Exit: you'd rather run porkchop than `meat | less` to read a reading diff; all three testdata goldens render correctly.*

**Phase 2 — JetBrains-class presentation.** Old │ new split alignment + intra-line token highlighting (the delegated pure module), `u` unified/split toggle, and navigation (hunk/file jumping, file strip, search). Now the remaining UI phase; note it must carry elision markers and expansion through into the split layout (fold rows span both columns, per Risks). *Exit: review a 15-file change with the keyboard only; changed tokens pop.*

**Phase 3 — trust features (the product). ✅ DONE (2026-08-17).** Built before Phase 2, since nothing here depends on split view. `diffview.Align` implements approach (a): a greedy forward alignment ported from `elision.go`'s `retainedDiffStats`, including its projection matcher for partially elided rows, emitting an `Elision` per gap (raw range, changed-line count, owning file, marker anchor, fold-row attribution). It is total — never errors, never panics — and a failed match widens a gap rather than corrupting the map, so divergence degrades toward "more is hidden", the safe direction. `internal/ui` grew synthesized elision markers, in-place expansion (`e`/`E`) behind a dimmed `│` rail, elision stepping (`n`/`p`), the audit view (`a`), and a "N hidden in M spots" header tile. Tested on all three goldens: alignment partitions the raw diff, every changed reading row finds its original, the hidden-line count **matches `meat.ElisionLine`'s own accounting exactly**, expansion reveals verbatim originals and collapses back byte-identically, and the audit view contains every hidden changed line. With no original available (`-reading-diff` without its `.diff` sibling) the screen degrades cleanly to Phase 1 behavior. *Exit met: "what did it hide, and was it right?" is `a`, or `E` in place.*

**Phase 4 — workflow.** `porkchop process`, hook installer, range review (`main...HEAD` across an agent session), per-file "viewed" markers.

**Phase 5 — polish/optional.** Chroma syntax highlighting inside add/context rows (must not fight diff colors), themes, `--serve` mode reusing the HTML viewer for sharing, upstream PR for plan exposure (approach (b)).

**Live-enablement (parallel prerequisite for any real run).** `internal/bedrock`: a CUI-compliant `meat.Model` over AWS Bedrock Converse, wired into `cmd/porkchop`'s compute path. Independent of Phases 1–3 (which run on fixtures); required before pointing porkchop at real CUI repos.

## Risks and open questions

- **Upstream velocity:** meat is young; the `Result` schema and cache format may churn. The renamed-module fork with a pristine `meat/` keeps sync cost low; the rubric hash already auto-invalidates caches across upgrades.
- **Split-alignment edge cases:** meat's folds straddle what a naive aligner expects — treat fold rows as full-width rows spanning both columns (simplest and honest).
- **Terminal width:** split wants ~180 cols; the `u` unified fallback (and unified-first Phase 1) is why this isn't a blocker for narrow CUI sessions.
- **CUI/Bedrock:** the whole live path depends on a compliant Bedrock backend (above). Cost/latency is inherited from meat and mitigated by the same caching.
- **Plan exposure:** Phase 3's approach (a) avoids a core change; approach (b) might be better contributed upstream — Bold Software may simply take it, shrinking the fork to pure UI.

## Name

Porkchop is right: it's meat, cut and plated properly. (Runner-ups if ever wanted: `carve`, `butcher`, `ribeye`.)

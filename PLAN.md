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

**Body — Phase 2 (JetBrains split).** Old │ new split of the reading diff with **intra-line token highlighting** (bright background on the differing tokens, standard red/green elsewhere), a line-number gutter per column, and a `u` toggle to unified for narrow terminals. Navigation: hunk stepping, file stepping, a file breadcrumb in the header rule, and `/` search with smart case and `n`/`N` stepping.

**Trust — Phase 3 (the point of the tool).** `e` expands the elision under the cursor **in place** (dimmed, behind a `│` rail — "the model called this noise"), `e` again re-collapses; `E` expands or collapses all; `n`/`p` step between elisions. `a` flips to an **audit view** showing *only* what was elided, grouped by file. This is where the original content surfaces — on demand, in flow. No web viewer, pager, or meat itself offers this.

**Load-bearing finding (2026-08-17): meat's `...` fold rows are not where the elision lives.** Measured across all three goldens, meat emits 1–2 fold rows per commit but leaves 14–27 distinct gaps that hide changed lines (62–90 changed lines total); the largest single unmarked gap in the pytest golden hides **25 changed lines**. Keying expansion to fold rows — as this plan originally specified — would therefore have exposed roughly a twentieth of the hidden content, which is not a basis for trust.

So the unit of expansion is the **alignment gap**, not the fold row, and porkchop **synthesizes** a marker wherever meat dropped changed lines silently. A fold row, where meat did emit one, is recorded as an attribute of the gap it marks and its marker stands in for it (the marker says "12 changed lines hidden", which strictly dominates what `...` conveys).

Gaps that hid *only* context get no marker and are omitted from the audit listing, reported as counts instead. That follows from meat being subtractive: the reading diff's lines are a subsequence of the original's, so no line can be reclassified, and a context line is by definition identical in the old and new versions. A gap with zero `+`/`-` lines therefore **cannot conceal any part of the change** — it is exactly the comprehension padding meat exists to drop.

**Commit picker (Phase 2/4).** A `git log` list with a per-commit status glyph: processed (cache hit, opens instantly), processing (spinner fed by meat's `Progress`), unprocessed. Accepts everything meat's CLI accepts and jumps straight to review.

Keybindings, JetBrains-flavored: `j/k` scroll, `g/G` top/bottom, `n/p` elision, `n/N` search match while a search is live, `]/[` file, `}/{` hunk, `e`/`E` expand, `a` audit, `/` search, `u` unified/split, `esc` unwinds one layer (search, then audit, then quit), `q` quit. Mouse wheel works for free in Bubble Tea. Elision stepping and anchor stepping **clamp** at the ends so a held key settles; search stepping **wraps**, as a search is expected to.

**Pinned for later — change-overview commentary.** An area of LLM-generated prose about the change: concise per-area descriptions and a breakdown of the overall change (the useful spirit of the prototype's hand-written "Preserved well / Budget critique" sidebar, but *generated*). This is a second, cheaper LLM pass — not free from `meat.Result`, and its exact content will evolve. We'll already be running an LLM (via Bedrock), so it's natural to add. **Deliberately deferred**; parked here so it isn't lost.

## Rendering pipeline (internal/diffview)

`diffview` is a **pure transform**: it takes the diff strings and emits a **semantic row model** — row kind (`meta`/`hunk`/`add`/`del`/`fold`/`context`), cell content, and (later) intra-line span roles and fold→raw-range mappings. It does **no** Lip Gloss styling and needs no terminal; **all styling lives in `internal/ui`**. This keeps the hardest logic unit-testable against fixtures with a binary pass/fail signal, and confines every visual/taste decision to one place. (This overrides the earlier "style with Lip Gloss in diffview" note.)

- Parse **leniently**: meat documents that `@@` hunk counts go stale after line removal — never trust the counts, read until the next header. Classification ports from the HTML prototype.
- Phase 1 is unified: parse → classify → rows. No alignment needed.
- Phase 2 adds split alignment: within a hunk, context rows align 1:1; paired `-`/`+` runs align row-by-row with blank filler for unequal lengths; **fold rows are full-width rows spanning both columns** — but they do *not* break the block they sit inside, or the runs either side of them pair against the wrong lines (see Phase 2). The same pairing feeds intra-line highlighting: tokenize into identifier / whitespace / **operator** runs, trim the common prefix and suffix, token-level LCS on what is left, emit spans.
- Tokenizing operator punctuation as a *run* rather than per character is load-bearing: with `=` split into single characters, the common subsequence legitimately matches the trailing `=` of both `==` and `!=`, and the reviewer is shown a highlight on `=` alone — accurate about tokens and misleading about the change.
- The correctness property the intra-line pass is tested against: spans mark exactly the tokens outside a longest common subsequence, so **deleting the spanned ranges from the old line and from the new line must leave two byte-identical strings.** Any drift — an off-by-one, a mismerged run, a bad backtrack — breaks that equality. It holds across all three goldens.

Note meat core has **zero dependencies** — keep it that way. Bubble Tea / Lip Gloss / bubbles deps belong to `cmd/porkchop` and `internal/`; `internal/diffview` stays pure stdlib; the `meat/` tree stays mergeable and upstreamable.

**TUI stack: Charm v2** (`charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`). Phase 1 built on v1 because v2 was still pre-release; it went GA before Phase 2, and migrating first meant touching ~770 lines of `internal/ui` rather than roughly double that afterward. Consequences to know: `View()` returns a `tea.View` and declares alt-screen/mouse itself (the `WithAltScreen`/`WithMouseCellMotion` program options are gone), key messages are `tea.KeyPressMsg`, and Lip Gloss colors are `image/color.Color` with no `AdaptiveColor` — `internal/ui` resolves a light/dark pair explicitly via `lipgloss.LightDark`, seeded dark and corrected from `tea.BackgroundColorMsg`, which keeps offline golden rendering deterministic. Requires Go ≥ 1.25.

**Two v2 facilities this plan expected to cash in Phase 2 turned out not to fit (2026-08-17).** Recorded because the migration's stated rationale rested on them:

- **`viewport.SetHighlights` is unusable for `/` search here.** `viewport.parseMatches` walks graphemes of `ansi.Strip(content)` while indexing byte positions into the *unstripped* `content`, so the two agree only when the content carries no escape sequences. Probed directly: plain content highlights correctly, pre-styled content collapses the ranges to empty escapes at end-of-line and loses the highlight silently. Porkchop's body is necessarily ANSI-bearing, because sub-line styling is what intra-line token highlighting *is*. So match painting moved into `internal/ui`'s render pass, where it became one more span layer beside token emphasis — a strictly simpler outcome than two highlighting mechanisms. `EnsureVisible(line, colstart, colend)` is public and still does the scrolling half.
- **`LeftGutterFunc` is the wrong shape for a two-column view.** It provides one gutter at the far left, but the split view needs a line number per column, and its advantage — surviving horizontal scrolling — does not apply because porkchop clips lines to the terminal width instead of scrolling them. Baking both gutters into the content is one code path for both views, and keeps the whole rendered surface inside `renderBody()` where the tests can assert on it.

`SoftWrap`, the `image/color` palette work, and general currency still justify the migration; these two do not.

## Workflow integration

`porkchop process [rev|range]` runs meat headlessly and fills the cache — the post-commit-hook / agent-devtool entry point. Cache layout: reuse meat's `~/.meat` content-addressed store untouched (same key = free interop with plain meat). `porkchop hook install` writes the post-commit hook. Later: a small `fsnotify` watcher that pre-processes new commits in open repos.

The planned `<key>.porkchop.json` sidecar was **not built, and should not be**. Phase 3's approach (a) recomputes the alignment from the two strings rather than persisting a plan, so there is no plan or audit metadata to keep; and the one piece of state porkchop does own — "viewed" markers — must specifically *not* be keyed by cache key. See Phase 4.

## Execution / how the work divides

The two-string seam sets the decomposition. The dependency graph is three **leaves** — `gitx`, `store`, `diffview` (each depends only on meat + stdlib) — and two **consumers** — `ui` → `diffview`, and `cmd/porkchop` → all. Slice work by package seam: each unit's working set is its own package + the two-string contract + `meat.go`'s ~100-line public API. Nothing ever needs to load meat's ~12k-line internals.

Recurring pattern across phases: **hard/pure/testable logic → `diffview`; visual/interactive/judgment → `ui`; plumbing → `gitx`/`store`/`cmd` (quick).** The pure work is fixture-testable against `meat/testdata` (`<name>.diff` = raw input, `<name>.golden.diff` = reading diff, `<name>.plan.json` = the compiled plan — which also prototypes the Phase-3 sidecar), which is what makes it delegable to a subagent with a test spec. Phases 2 and 3 were both hand-built in the end; the split held up regardless, and the fixture tests are what caught the fold-row mispairing and the two-left-edges bug.

## Phases

**Phase 0 — fork mechanics. ✅ DONE (2026-08-04).** Module renamed; `meat/` pristine; `upstream` remote added + fetched. `internal/gitx` and `internal/store` are self-contained copies of `cmd/meat`'s git + cache plumbing (so `cmd/meat` stays byte-pristine); `store.Key` is identical to meat's, so cache interop is guaranteed. `cmd/porkchop` skeleton accepts meat's args, shares the cache, renders plain text. `NOTICE` records the Apache-2.0 lineage. `internal/store` unit-tested. Build/vet/tests green. On branch `phase-0-fork`.

**Phase 1 — unified reading-diff TUI.** `internal/diffview` parses `SmartDiff` into classified semantic rows; `internal/ui` renders one column (green/red/amber), the header (summary + stat manifest), viewport scroll (`j/k`, mouse), `q`. Reads cache or computes (compute needs Bedrock/creds; **develop and test against `meat/testdata` goldens, no LLM**). *Exit: you'd rather run porkchop than `meat | less` to read a reading diff; all three testdata goldens render correctly.*

**Phase 2 — JetBrains-class presentation. ✅ DONE (2026-08-17).** `diffview.Split` emits the two-column layout and the intra-line token spans, pure and fixture-tested; `internal/ui` renders both views from one span-painting path. *Exit met: a 15-file change reviews from the keyboard alone, and changed tokens pop.* (Claimed on six-file fixtures; **verified at twenty in Phase 4**, which found one real defect at that size — see there.) What landed, and the three findings worth carrying forward:

- **Pairing is positional**, per this plan: `dels[i]` opposite `adds[i]`, filler under the longer run. A similarity-matched pairing would invent correspondences the diff does not contain, which a review tool must not do. The intra-line pass compensates honestly instead — a **similarity gate** drops token highlighting for a pair that shares too little, so a bad positional pair degrades to plain red/green rather than to noise.
- **A fold row must not split a change block.** Doing so paired `apply_warning_filters(...)` against `with config._catch_configured_warnings(...)` in the pytest golden — two lines with nothing to do with each other — while the true pair never met. Folds are now remembered and re-emitted full width at the point in the block they interrupted. This is the "folds straddle what a naive aligner expects" risk, and it was real.
- **Line numbers can only come from the raw diff.** meat elides lines without renumbering the `@@` headers it leaves behind, so counting forward inside a reading diff drifts by the size of every gap it passes. `diffview.Alignment.Nums` carries exact numbers taken from the raw side; a row porkchop cannot place is left blank, and with no original supplied the gutter is omitted entirely. A plausible-looking wrong line number is worse than none in a tool whose product is justified trust.

Keys added: `u` unified/split, `]`/`[` file, `}`/`{` hunk, `/` search. `n`/`N` step matches while a search is live and elisions otherwise, and the footer always names which. The file strip/tree was **replaced** by a breadcrumb in the header rule (`path (3/15)`), which costs no vertical space and is what `]`/`[` needs to be legible; a tree pane is deferred and probably unnecessary. The search prompt is hand-rolled rather than `bubbles/textinput`, which imports a clipboard library that shells out to `pbcopy`/`xclip` — an unannounced subprocess is a bad trade for editing niceties in a CUI enclave. **Phase 2 added no dependencies.**

**Phase 3 — trust features (the product). ✅ DONE (2026-08-17).** Built before Phase 2, since nothing here depends on split view. `diffview.Align` implements approach (a): a greedy forward alignment ported from `elision.go`'s `retainedDiffStats`, including its projection matcher for partially elided rows, emitting an `Elision` per gap (raw range, changed-line count, owning file, marker anchor, fold-row attribution). It is total — never errors, never panics — and a failed match widens a gap rather than corrupting the map, so divergence degrades toward "more is hidden", the safe direction. `internal/ui` grew synthesized elision markers, in-place expansion (`e`/`E`) behind a dimmed `│` rail, elision stepping (`n`/`p`), the audit view (`a`), and a "N hidden in M spots" header tile. Tested on all three goldens: alignment partitions the raw diff, every changed reading row finds its original, the hidden-line count **matches `meat.ElisionLine`'s own accounting exactly**, expansion reveals verbatim originals and collapses back byte-identically, and the audit view contains every hidden changed line. With no original available (`-reading-diff` without its `.diff` sibling) the screen degrades cleanly to Phase 1 behavior. *Exit met: "what did it hide, and was it right?" is `a`, or `E` in place.*

**Phase 4 — workflow. ✅ DONE (2026-08-18).** `porkchop process`, the hook installer, range review, and per-file "viewed" markers. *Exit met: after `hook install`, a session's commits are already processed by the time anyone looks; `porkchop main...HEAD` opens on a cache hit; and files checked off stay checked as the agent keeps committing.* What landed, and the decisions worth not relitigating:

- **A viewed marker is keyed to the file, not to the change.** `diffview.Alignment.Digests` hashes each file's own section of the *original* diff, and `internal/store`'s per-repo marker file records one `{digest, timestamp}` per path. The obvious alternative — a `<key>.porkchop.json` beside meat's cache entry — is a line of code and is wrong: the cache key is a hash of the whole diff, so every marker would reset the moment any file in the change moved. That is exactly the case the feature exists for, reviewing `main...HEAD` while an agent keeps committing. Digests come from the original rather than the reading diff because a hash of the abridgement also moves when the model or rubric changes, silently un-viewing files for a reason that has nothing to do with them. With no original supplied there is no digest, and markers degrade to session-only rather than being recorded against something unverifiable.
- **The hook is backgrounded, logged, and incapable of failing a commit.** `nohup ... &` inside a subshell with stdin closed and both streams appended to `$GIT_DIR/porkchop-process.log`; every path in the block returns 0. A tool that can delay or break `git commit` gets uninstalled in a week. It is installed as a marked block via `git rev-parse --git-path hooks` (so `core.hooksPath` and worktrees are honored — guessing `.git/hooks` installs a hook git will never run, which fails silently), so an existing post-commit hook keeps working and `uninstall` removes only porkchop's part.
- **The replay-storm guard was built, measured, and deleted.** The prediction was that a rebase would fire the hook once per replayed commit. Measured: git does not run `post-commit` for rebase *or* cherry-pick at all. It does for `--amend` and for merge commits, both of which are worth a reading diff. So the guard was dead code that, had it ever fired, would have suppressed the merge commits we want — and it is gone.
- **`process` treats an empty diff as success**, while the review command still errors on one. A hook fires on every commit including the empty ones; a human who asked to read a diff should be told there is not one.
- **The workflow commands accept flags on either side of the verb** (`hook install -force`, `process HEAD -json`). Go's `flag` stops at the first non-flag word, which makes the order people actually type into a silent usage error. The review command keeps `flag`'s default behaviour, because it has to accept meat's arguments exactly as meat does.
- **Scale, at last: navigation is exercised to twenty files.** The largest real fixture is six, so `internal/ui`'s scale tests generate a synthetic twenty-file, sixty-hunk change — no claim about what meat's output looks like, only about what the navigation must survive. It immediately found a real defect: **the last screenful of a long change was unreachable as a scroll position.** The viewport correctly refuses to scroll past its content, so the final files could never reach the top, the breadcrumb could never name them, and `v` could never check them off — invisible on a six-file fixture that fits the screen. The body is now followed by blank lines so any line can reach the top, `G` scrolls to the end of the *content* rather than the padding, and the scroll percentage is measured against the body so the tail does not read as 60%.
- **The footer picks its hint list by measurement, not by a column threshold.** Four tiers, tried widest-first against the rendered width. The old two-tier version clipped at exactly 100 columns — the hints interpolate counts like `n/p elision (240/1200)`, so any hardcoded threshold is wrong for some change, and a clipped footer loses its tail, which is where `q quit` lives.

Keys added: `v` toggles the current file, `tab` jumps to the next unviewed one. `tab` is the only stepping key that wraps, because the unviewed set shrinks as the reviewer works and clamping would strand skipped files above the cursor. Progress shows as a check in the breadcrumb and a tile in the header, which names the finish line explicitly rather than leaving it to be inferred from two equal numbers. **Phase 4 added no dependencies.**

**Phase 5 — polish/optional.** Chroma syntax highlighting inside add/context rows (must not fight diff colors), themes, `--serve` mode reusing the HTML viewer for sharing, upstream PR for plan exposure (approach (b)).

**Live-enablement (parallel prerequisite for any real run).** `internal/bedrock`: a CUI-compliant `meat.Model` over AWS Bedrock Converse, wired into `cmd/porkchop`'s compute path. Independent of Phases 1–3 (which run on fixtures); required before pointing porkchop at real CUI repos.

## Risks and open questions

- **Upstream velocity:** meat is young; the `Result` schema and cache format may churn. The renamed-module fork with a pristine `meat/` keeps sync cost low; the rubric hash already auto-invalidates caches across upgrades.
- ~~**Split-alignment edge cases:**~~ **Confirmed real and resolved (Phase 2).** Fold rows are full-width, but re-emitted inside the block they interrupted rather than breaking it; breaking it mispaired lines in the pytest golden.
- ~~**Terminal width:** split wants ~180 cols~~ **— measured, and it is less.** Split is chosen at ≥120 columns, which leaves ~53 cells per column at four-digit line numbers; below that, or below 24 cells per column, porkchop opens unified. `u` overrides either way and the choice is then pinned, so a resize never undoes it.
- **CUI/Bedrock:** the whole live path depends on a compliant Bedrock backend (above). Cost/latency is inherited from meat and mitigated by the same caching.
- **Plan exposure:** Phase 3's approach (a) avoids a core change; approach (b) might be better contributed upstream — Bold Software may simply take it, shrinking the fork to pure UI.

## Name

Porkchop is right: it's meat, cut and plated properly. (Runner-ups if ever wanted: `carve`, `butcher`, `ribeye`.)

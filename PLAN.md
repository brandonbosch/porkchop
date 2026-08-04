# porkchop — plan for forking meat into a TUI-first reading-diff reviewer

*A fork of [boldsoftware/meat](https://github.com/boldsoftware/meat): same trustworthy abridging core, presented the way a reviewer actually wants to consume it.*

## Thesis

Meat solved the hard problem — turning an AI-generated diff into a trustworthy reading diff via subtractive edit plans — and then handed the result to `less`. Porkchop keeps meat's core untouched and replaces the presentation layer with a Bubble Tea / Lip Gloss TUI: side-by-side panes, keyboard stepping, intra-line highlighting, and interactive un-hiding of everything the model elided. The design principle carried over from meat: the model supplies judgment, the machine supplies integrity. Porkchop adds a third leg: the reviewer supplies attention, and the UI should spend it well.

## Fork strategy

Fork the repo, but treat `meat/` (the core package) as vendored upstream code you never modify except through upstream merges. All porkchop code lives in new packages. This keeps `git merge upstream/main` clean — the upstream is days old and moving fast, and you want their rubric/compiler improvements for free. License is Apache 2.0: keep their copyright notice, add a NOTICE file stating modifications, relicense your additions as you like (Apache 2.0 for the whole is simplest).

Mechanics: `git clone`, add `upstream` remote, rename the module (`meat.dev` → `github.com/<you>/porkchop`) — this touches import paths in `cmd/` and `meat/`, which slightly complicates upstream merges. Two mitigations: do the rename in one dedicated commit so merges conflict predictably, or (cleaner) use a `go.mod` `replace` directive and keep `meat.dev` paths intact inside `meat/`. Prefer the replace directive; it makes upstream syncs nearly conflict-free.

Repo layout:

```
porkchop/
  meat/                 # upstream core, pristine (LLM planner + edit-plan compiler)
  cmd/meat/             # upstream CLI, pristine (still works; useful for diffing behavior)
  cmd/porkchop/         # new: the TUI binary + `porkchop process` subcommand
  internal/ui/          # Bubble Tea models, Lip Gloss styles, keymaps
  internal/diffview/    # diff parsing, side-by-side row alignment, intra-line spans
  internal/store/       # cache: read/write meat results + porkchop sidecar metadata
  internal/gitx/        # git log/show/range plumbing (adapted from cmd/meat/main.go)
```

## What porkchop adds (and what it deliberately doesn't)

Porkchop is a renderer and workflow around `meat.Abridge`. It does not touch the rubric, the edit-plan compiler, move detection, import stripping, or chunking. Every pixel porkchop draws comes from the original diff plus meat's subtractive plan — the fork inherits meat's core honesty guarantee, and the UI can prove it interactively (see fold expansion below).

The one core-adjacent change worth making: meat discards the model's edit plan after applying it, keeping only the final `SmartDiff`. Porkchop wants the plan (for fold expansion and the audit view), so `internal/store` caches a sidecar next to meat's result: the compiled plan's remove/fold/replace ranges mapped to original-diff line numbers. Two ways to get it: (a) recompute by diffing `SmartDiff` against the original — pure, no core changes, slightly lossy; or (b) a three-line addition to `meat.Result` exposing the applied plan — cleaner, and a good candidate to offer upstream as a PR before carrying it as a fork patch. Start with (a), pursue (b).

## TUI design

Two screens, modeled on the HTML viewer we built plus JetBrains habits.

**Commit picker.** A `git log` list with a status glyph per commit: processed (cache hit, opens instantly), processing (spinner fed by meat's `Progress` callback), unprocessed. Enter opens review; `p` queues processing. Also accepts everything meat's CLI accepts (`porkchop`, `porkchop <rev>`, `<range>`, `-staged`, `-w`, stdin) and jumps straight to review.

**Review screen.** Header: meat's one-line summary plus the locally computed elision manifest ("kept 83/109 changed rows · 29% removed"). Body: two synced viewports — original left, reading diff right — with a `u` toggle for unified single-pane (small terminals). A file strip across the top (or a collapsible tree on `t`) shows per-file retention so you can see at a glance where the meat is. Right sidebar (toggle `i`): per-file stats and the fold inventory.

Keybindings, JetBrains-flavored: `j/k` scroll, `J/K` or `n/p` next/prev hunk, `]/[` next/prev file, `tab` switch pane focus, `e` expand/collapse fold under cursor, `E` expand all, `a` audit view, `/` search, `s` sync-scroll toggle, `q` quit. Mouse wheel works for free in Bubble Tea.

**Fold expansion — the killer feature.** Because meat's plan is subtractive, every amber `...` row maps to a known range of real lines from the original diff. `e` on a fold expands it in place (dimmed styling to show "the model called this noise"); `e` again re-collapses. This is the interactive answer to "do I trust the elision?" — the reviewer can always look, cheaply, without leaving flow. No web viewer, pager, or meat itself offers this today.

**Audit view.** `a` flips to showing *only* what was elided, dimmed, grouped by file. Skimming the discard pile for 10 seconds is how a senior engineer learns to trust (or calibrate) the tool. This is also your answer to the audit-trail gap we identified in meat's cache.

**Intra-line highlighting.** For paired changed lines, highlight the changed spans: bright background on the differing tokens, standard red/green elsewhere (JetBrains/delta style). Implementation: within each hunk, pair `-` runs with `+` runs positionally; for each pair, tokenize (words + punctuation) and run Myers/LCS at token level; emit styled spans. Meat's `...` elisions tokenize like any other token and pair up naturally. ~150 lines; test against delta's fixtures for sanity.

## Rendering pipeline (internal/diffview)

Parse the reading diff leniently — meat documents that hunk counts go stale after line removal, so never trust `@@` counts, just read until the next header. Classification logic ports from the HTML viewer (meta/hunk/add/del/fold/context). Side-by-side alignment: within a hunk, context rows align 1:1; paired -/+ runs align row-by-row with blank filler for unequal lengths — the same alignment that feeds intra-line highlighting. Style with Lip Gloss using the viewer's palette (it's a good palette): adaptive to terminal background, `NO_COLOR`/`-plain` respected. Optional syntax highlighting via chroma inside add/context rows is a Phase 5 nicety; don't let it fight the diff colors.

Note meat core has zero dependencies — keep it that way. Bubble Tea/Lip Gloss/bubbles deps belong to `cmd/porkchop` and `internal/`, so the `meat/` tree stays mergeable and upstreamable.

## Workflow integration

`porkchop process [rev|range]` runs meat headlessly and fills the cache — this is the post-commit hook / agent-devtool entry point, replacing "have your agent build meat into your devtools." Cache layout: reuse meat's `~/.meat` content-addressed store untouched (same key = free interop with plain meat), adding `<key>.porkchop.json` sidecars for plan/audit metadata. A `porkchop hook install` convenience writes the post-commit hook. Later: a tiny `fsnotify` watcher mode that pre-processes new commits in any repo you have open.

## Phases

**Phase 0 — fork mechanics (an evening).** Fork, `replace` directive, `cmd/porkchop` skeleton that just calls the existing pipeline, CI, NOTICE file. Exit criteria: `porkchop <sha>` behaves exactly like `meat <sha>`.

**Phase 1 — static TUI renderer (a weekend).** Bubble Tea app: review screen, two synced viewports, colored classification, header summary + manifest, `j/k`/`q`. Reads cache or computes with meat's progress feeding a spinner. Exit: you'd rather run porkchop than `meat` for a real review.

**Phase 2 — navigation (a week of evenings).** Hunk/file jumping, file strip, unified toggle, search, commit picker with cache status. Exit: you can review a 15-file agent PR without touching the mouse or scrolling past anything twice.

**Phase 3 — trust features.** Plan sidecar (approach (a)), fold expansion, audit view, intra-line highlighting. This phase is the product. Exit: you can answer "what did it hide and was it right?" in under a minute on any commit.

**Phase 4 — workflow.** `porkchop process`, hook installer, range review (`main...HEAD` across an agent session), maybe a review checklist marker per file (JetBrains' "viewed" checkbox).

**Phase 5 — polish/optional.** Chroma syntax highlighting, themes, `--serve` mode reusing the HTML viewer for sharing, upstream PR for plan exposure.

## Risks and open questions

Upstream velocity: meat is ~24 commits old; the `Result` schema and cache format may churn. The replace-directive fork and untouched `meat/` tree keep sync cost low; the rubric hash already auto-invalidates caches across upgrades. Side-by-side edge cases: meat's folds can straddle what a naive aligner expects — the alignment code needs fold rows treated as full-width rows spanning both panes (simplest and honest). Terminal width: side-by-side wants ~180 cols; unified fallback is why the `u` toggle exists from Phase 1. Cost/latency is inherited from meat and mitigated by the same caching; nothing UI-side changes it. Finally, consider whether Phase 3's plan-exposure change is better contributed upstream than carried — Bold Software may simply take it, which shrinks your fork to pure UI.

## Name

Porkchop is right: it's meat, cut and plated properly. (Runner-ups if you ever want them: `carve`, `butcher`, `ribeye`. But porkchop has the right amount of not taking itself seriously.)

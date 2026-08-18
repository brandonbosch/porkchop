# porkchop

**Read agent-written diffs the way you'd read prose — the important parts, in a
TUI, with the original one keystroke away.**

porkchop is a fork of [meat](https://github.com/boldsoftware/meat) that keeps
meat's abridging core intact and replaces its `less` output with a real
reviewer: a split-diff terminal UI with navigation, search, per-file progress,
and in-place expansion of everything the model chose to hide.

## Why

Models write a lot of code now, and most of a generated diff is not worth a
human's attention: imports, nil checks, formatting, the mechanical half of a
refactor. What *is* worth attention — the concept, the algorithm choice, the
thing that will be wrong in six months — is buried in it.

meat's insight is to spend a model on *reducing* the diff instead of writing
more of it. The output is a **reading diff**: the same change, abridged, with
the skipped runs marked rather than deleted.

porkchop's addition is that an abridgement you cannot check is a claim, not a
review. So the pre-meat original is never more than a keystroke away — `e`
expands one elision in place, `a` shows the full original beside the
abridgement. That on-demand verification is the load-bearing feature, not a
convenience.

## Install

```bash
go install github.com/brandonbosch/porkchop/cmd/porkchop@latest
```

Needs Go 1.26.5 or newer.

## Quickstart

porkchop takes git-shaped arguments, like `git show`:

```bash
porkchop                    # the latest commit
porkchop HEAD~3             # a specific commit
porkchop main..feature      # a range
porkchop -w                 # unstaged working-tree changes
porkchop -staged            # staged changes
```

Before any of that works, it needs a model. **See [Choosing a
backend](#choosing-a-backend) — there is no default that works out of the
box, deliberately.**

On a terminal you get the review screen. With stdout redirected you get plain
text, so pipes still work:

```bash
porkchop HEAD~1 > review.txt
porkchop -json HEAD~1 | jq .summary
```

## Reviewing

The footer always names the keys that matter at your terminal's width. The
full set:

| Key | Does |
| --- | --- |
| `j` / `k` | Scroll |
| `g` / `G` | Top / bottom |
| `]` / `[` | Next / previous file |
| `}` / `{` | Next / previous hunk |
| `n` / `p` | Next / previous elision marker |
| `e` | **Expand the current elision in place** |
| `E` | Expand or collapse everything |
| `v` | Mark the current file viewed |
| `tab` | Jump to the next unviewed file |
| `/` | Search; then `n` / `N` step matches, `esc` clears |
| `u` | Toggle split / unified |
| `a` | **Audit view — the full original beside the abridgement** |
| `q` | Quit |

Two of those are the point of the tool. `e` expands what the model hid, right
where it was hidden, so checking a judgment call costs one key and never loses
your place. `a` is the escape hatch for when you don't trust the abridgement at
all and want to read the raw thing.

Per-file **viewed** markers persist under the cache directory, keyed to each
file's own content — so they survive a range growing as an agent keeps
committing, and reset only for files that actually changed.

## Choosing a backend

porkchop assumes everything it sends a model is sensitive: the diff, plus
whatever surrounding source meat's `read_file` and `grep` tools pull in while
it works. So **the default backend is AWS Bedrock, and reaching a public API is
something you ask for by name.** There is no configuration that quietly
egresses.

| Provider | What it is | Needs |
| --- | --- | --- |
| `bedrock` *(default)* | Claude on AWS Bedrock, commercial or GovCloud | A Bedrock **inference profile id**, a region, and either the AWS credential chain or `$AWS_BEARER_TOKEN_BEDROCK` |
| `anthropic` | The public Anthropic API | `$ANTHROPIC_API_KEY` |
| `openai` | The public OpenAI API | `$OPENAI_API_KEY` |
| `openai-compat` | Any OpenAI-compatible endpoint — Ollama, llama.cpp, LM Studio, oMLX, vLLM | `-base-url`, a model id, and `$PORKCHOP_API_KEY` if the server enforces one |

### Presets

Retyping four flags is how people end up with a shell alias that outlives the
setting it was written for. Instead, name a backend once:

```bash
porkchop -write-config      # writes ~/.config/porkchop/config.json
```

Edit it, then switch backends with one word:

```bash
porkchop -preset omlx HEAD~1
```

A preset is just stored flags:

```json
{
  "default_preset": "",
  "presets": {
    "work": {
      "provider": "bedrock",
      "model": "us-gov.anthropic.claude-sonnet-4-5-20250929-v1:0",
      "region": "us-gov-west-1"
    },
    "laptop": {
      "provider": "openai-compat",
      "model": "Ornith-1.0-35B-4bit-mlx",
      "base_url": "http://localhost:8888/v1",
      "api_key_env": "OMLX_API_KEY"
    }
  }
}
```

Resolution order is **flags > environment > preset > built-in default**, so a
preset never overrides something you typed. Set `default_preset` to pick one
without the flag, or export `$PORKCHOP_PRESET`.

Keys are never stored in the file — `api_key_env` names an environment variable
to read instead. Presets are read from *your* config only; porkchop never looks
for a config file in the repository under review, because the code being
reviewed does not get to choose where its own diff is sent. For the same
reason, the config file is refused if other users can write to it.

Every live call prints where it is going before it goes:

```
porkchop: using openai-compat http://localhost:8888/v1 (Ornith-1.0-35B-4bit-mlx)
```

Cache hits print nothing, because nothing leaves the machine.

### A note on Bedrock model ids

`-model` wants a Bedrock **inference profile id** — an AWS resource whose id
*is* the model id — not the `~/.aws` named profile that `$AWS_PROFILE` selects.
The partition is part of the id and is passed through verbatim, so a commercial
id used in GovCloud fails rather than being corrected:

- commercial: `us.anthropic.claude-sonnet-4-5-20250929-v1:0`
- GovCloud: `us-gov.anthropic.claude-sonnet-4-5-20250929-v1:0`

List what you have access to with:

```bash
aws bedrock list-inference-profiles --region <region>
```

### Local models

Local inference works through `openai-compat`. Two things to expect that differ
from a hosted model:

- **Tool calling is required, not optional.** meat's core is an agent — it
  submits its abridgement *through* a tool call. A model that cannot reliably
  emit tool calls will not work at all, regardless of how well it writes.
- **Output tokens dominate.** On a reasoning model most of the spend is
  thinking, not answer. A 28-line diff can cost ~13k output tokens locally
  against ~500 on a hosted model. The per-turn cap is 16384 and a truncated
  reply is a hard error, so very large diffs are where a local model gives out
  first.

## Warming the cache

Abridging takes real time — it is a model call, sometimes several. You do not
want to wait for it when you sit down to review. So compute it at commit time
instead:

```bash
porkchop hook install       # runs "porkchop process HEAD" after each commit
porkchop hook status
porkchop hook uninstall
```

The hook never fails a commit and never delays one — the model call is detached
and logged. It fires for ordinary commits, `--amend`, and merges; git does not
run `post-commit` for rebases or cherry-picks, so replaying fifty commits costs
nothing.

You can also call `porkchop process` directly, which is what an agent harness
should do when it finishes a change:

```bash
porkchop process HEAD
porkchop process -json HEAD       # cache key and token spend, for a harness
```

Results are cached under `~/.meat` with the same key meat uses, so a commit
processed by either tool is an instant hit for the other. The key covers the
diff, the model id, and meat's rubric — change any of them and it recomputes.

## Relationship to meat

`meat/` and `cmd/meat/` are vendored from upstream and kept byte-pristine apart
from the module path, so `git merge upstream/main` never conflicts there. The
`meat` CLI still builds and behaves exactly as it does upstream. Everything
under `cmd/porkchop/` and `internal/` is this fork's work.

porkchop and meat share the cache, so you can run either.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE) — meat is
Copyright 2026 Bold Software, Inc.

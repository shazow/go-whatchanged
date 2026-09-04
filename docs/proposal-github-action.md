# Proposal: the GitHub Action, and less shell under it

The action works: every pull request on this repository gets its comment,
and the dogfood job runs in four seconds with a warm cache. This proposal
is about what surrounds it. How the action is shared, `uses:
shazow/go-whatchanged@main` with `v1.0.0` since tagged; how the README asks a
project to adopt it; and the 283-line shell script that does everything
between `go build` and the comment, which is the part most likely to
break and least likely to be tested.

It rests on a survey of how comparable tools ship their actions, with
sources in the last section: `golangci-lint-action`, `goreleaser-action`,
`govulncheck-action`, `staticcheck-action`, `gosec`, the `reviewdog`
family, `cosign-installer`, `cargo-semver-checks-action`, which is this
tool's Rust counterpart, and `go-apidiff`, the one other Go API-diff
action. Three findings shape everything below.

- **Nobody builds from the action's own checkout.** Composite Go-tool
  actions run `go install pkg@version` with a `version` input; JavaScript
  actions download a release binary. Yet the survey turned up no reason to
  change, and one to stay: see [How it is shared](#how-it-is-shared).
- **Nobody moved CI logic into their binary, and one tool moved it back
  out.** `golangci-lint` deprecated its `--out-format=github-actions` in
  2024 and removed it in v2; `staticcheck`, `govulncheck`,
  `cargo-semver-checks` and `gorelease` have no GitHub mode at all. Job
  summaries, outputs and comments belong to the wrapper.
- **Every action is on the Marketplace with a floating major tag** (`@v2`,
  `@v3`), and GitHub's own guidance says users "should not be referencing
  an action's default branch".

## How it is shared

### Keep building from the pinned ref

The action builds the tool from `$GITHUB_ACTION_PATH`, so the ref that
pins the action is the version of the tool. No surveyed action does this;
`go-apidiff`, `staticcheck-action` and `govulncheck-action` run `go
install` and take a `version` input, and the JavaScript actions download a
release tarball. For this tool the unusual choice is the right one, for
three reasons the README does not give.

1. **A Go toolchain is needed at run time anyway.** The tool type-checks
   against `$GOROOT/src` and runs `go mod download` for modules the cache
   lacks. A prebuilt binary would not remove the toolchain requirement, so
   there is nothing to gain from a release binary that `go install` or
   `go build` does not give.
2. **The Go it is built with caps what it can type-check.** `--version`
   already says so. Building with the caller's toolchain, which is at
   least as new as their module's `go` directive or their own build would
   fail, is the one way to be sure the tool understands their code. A
   binary from a release would be frozen at the Go of the release.
3. **One pin.** The action's ref and the tool's version cannot drift
   apart, and a `version` input has nothing to select.

Two rough edges of this choice deserve a fix, and both are small.

- **The toolchain download.** `go.mod` says `go 1.26.0`, so a caller on an
  older Go pays for a toolchain download on every cold build, or fails
  under `GOTOOLCHAIN=local`. Nothing in the code needs 1.26; the newest
  features are `strings.SplitSeq` (1.24) and `sync.WaitGroup.Go` (1.25).
  Setting the directive to the oldest Go that builds the tool, and keeping
  it there, is worth a line in `CI` that builds with that Go.
- **The caller's environment leaks into the build.** `GOFLAGS=-mod=vendor`
  or a `GOWORK` pointing into the caller's repository breaks `go build` of
  the action. The build step should run with `GOFLAGS=` and `GOWORK=off`.

`actions/setup-go` before the action is not a requirement, only the README
presents it as one: GitHub's Ubuntu image ships Go 1.24, 1.25 and 1.26,
and `GOTOOLCHAIN=auto` picks up what `go.mod` asks for. `setup-go` is
worth recommending for its cache, which is what makes the dogfood build
take four seconds instead of thirty: it saves the build cache and the
module cache under the caller's key, and the action's build lands in
them. The README should say that, and say that the action works without
it.

### Tag releases, move a major tag, list on the Marketplace

`v1.0.0` is tagged, so `go install github.com/shazow/go-whatchanged@latest`
resolves to a release and the README can pin the action to it. Two
things remain.

- Keep a floating major tag, `v1`, moved on each release, as
  `actions/toolkit` documents (`git tag -fa v1 && git push origin v1
  --force`). Create it as a plain tag, not a release: with immutable
  releases enabled a tag tied to a release cannot move. A tag named `v1`
  is not a version to the go command, which lists only canonical semantic
  versions, so it costs the module nothing. A ten-line workflow on `push:
  tags: v*` can move it, and the README can then say `@v1` and stop
  needing an edit per release.
- Publish on the Marketplace. `action.yml` is at the root with `branding`,
  which is all the listing needs beyond a release.

Either way, mention pinning by SHA with a version comment, `uses:
shazow/go-whatchanged@<sha> # v1.0.0`, as kubebuilder pins `go-apidiff`
and GitHub's security guidance recommends.

A separate `go-whatchanged-action` repository, the layout of golangci-lint
and goreleaser, would only make sense with a release binary to download.
With build-from-ref, one repository and one tag is simpler for the
maintainer and the user alike.

## The README's instructions

The section is complete but leads with the wrong things. A project
deciding whether to adopt the action needs, in order: what it will see,
the smallest workflow that gets it, and then the knobs.

1. **Lead with the result.** A rendered example of the comment, folded
   under its summary line, and one sentence on the job summary. The
   markdown example under Output formats is close; the comment adds the
   glyph and the footer.
2. **One minimal workflow**, `on: pull_request`, three steps, with the
   comment `# for the comment` on the permission and `# history and tags,
   for the merge-base and @latest` on `fetch-depth`, as now. Pinned to
   `@v1`. `setup-go` in it with a comment that it is optional, for the
   cache.
3. **A cookbook** of the questions the Usage table asks, as workflow
   fragments: a compatibility gate (`fail-on: major`), the public API
   alone (`filter: public`), several modules (a matrix over
   `working-directory`), release notes on a tag push (`on: push: tags:
   v*`, which the current text mentions but whose snippet, `on:
   pull_request`, cannot do), and a pull request from a fork.
4. **Forks.** Today the README says the comment is skipped and the job
   summary still has the diff, which is right. The cookbook should add the
   `pull_request_target` recipe that Trivy uses for `go-apidiff`, with its
   caveat: the base repository's token can comment, and the tool only
   parses and type-checks the pull request's code, but `go mod download`
   still reads its `go.mod`, so the recipe is for maintainers who have
   thought about that.
5. **The tables** of inputs and outputs are fine as reference; move them
   below the cookbook and keep `action.yml` the source of truth for the
   descriptions.
6. **The plain-steps alternative** for other CI systems is useful; it
   should use `@origin/main...`, the merge-base spelling proposed below,
   once it exists, since `@origin/${{ github.base_ref }}` is wrong for any
   branch that is behind its base.

## The shell script, concern by concern

`.github/scripts/whatchanged.sh` is 283 lines and has been changed in 16
commits. What it does, and where each piece could live instead:

| ≈ lines | Concern | Today | Proposed home |
|---:|---|---|---|
| 30 | Pick the base: merge-base of head and the base branch, fetching the branch tip first, with a fallback to the tip on a shallow clone | `git merge-base`, `git fetch` | The tool: `@rev...` names a merge-base. The action fetches the branch and passes it. |
| 25 | Run the tool twice, JSON then markdown, and turn `warn:` lines on stderr into annotations | Parsing stderr by prefix | The JSON already carries `warnings`; read them from it. |
| 5 | Extract the summary line(s) from the markdown | `awk` over fences, headings and emphasis | The tool: a `text` field in the JSON summary. |
| 20 | Annotations, error text extraction | `sed`, `awk`, `%25` escaping | Stays in the action, in whatever language it is written. |
| 40 | Log group, job summary, outputs | `jq` and heredoc delimiters | Stays in the action. |
| 95 | The pull request comment: find by marker, truncate to 65536, post or update, explain a fork | `curl` and `jq` against the REST API | Stays in the action. |
| 15 | Final annotation and exit code | | Stays in the action. |

Two of these move into the tool, and both are features the command line
wants on its own merits.

### Native: `@rev...` for the merge-base

The README answers "what does this branch change, compared to main?" with
`go-whatchanged @origin/main`. That is right only while the branch is up
to date with main; once main moves, everything main added shows up as
removed, and the required release is wrong. The question wants the
merge-base, which is what the action computes with `git merge-base` and
the reason it needs the base branch fetched.

Git spells this `main...HEAD`: the changes on the second side since the
two diverged. The tool can take the same spelling as a revision:
`@origin/main...` is the merge-base of `origin/main` and the head, and
`@origin/main...HEAD~1` the merge-base of the two named revisions.
go-git computes it, `object.Commit.MergeBase`, without shelling out, so
the read-only guarantees hold. The error when the history does not reach
the merge-base already has the shallow-clone hint with the `fetch-depth:
0` form under `GITHUB_ACTIONS`.

With that, the action's base resolution becomes a fetch and a name: fetch
`origin/$GITHUB_BASE_REF` when the checkout lacks it, and pass
`@origin/$GITHUB_BASE_REF...`. Two refinements come with it.

- **Fetch the branch, not `base.sha`.** The event's `base.sha` is the tip
  of the base branch when the pull request was last updated, not now;
  Trivy's workflow notes this and fetches the branch by name. For a
  merge-base it rarely matters, but the branch is the right thing to name,
  and it is what the plain-steps alternative in the README already uses.
- **A shallow checkout has a better fallback than the tip.** On a
  `pull_request` event, `actions/checkout` checks out the merge commit
  `refs/pull/N/merge`, whose first parent is the base branch's tip and
  whose second is the pull request's head. `git fetch --deepen=1` brings
  both parents, and `@HEAD^1` against `@HEAD` is then "what merging this
  pull request changes on the base branch": exact, and free of the
  behind-main problem, even without `fetch-depth: 0`. That is arguably a
  better default than the merge-base for a review, and what Trivy chose
  deliberately for `go-apidiff`. The one thing it cannot do is `@latest`,
  which still needs the tags. The action could use it whenever the
  merge-base is out of reach, in place of the tip and its warning.
- **`merge_group` events** carry `merge_group.base_sha`; `go-apidiff`
  defaults to it alongside `pull_request.base.sha`. One more case in the
  same place.

### Native: the summary as text in the JSON

The action needs the summary line for the annotation, the `summary`
output and the comment's fold line, and gets it by running the markdown
layout through `awk` that knows which lines are fences, which are
headings and which carry emphasis. The last layout change had to teach it
`###` in place of bold, and the next one will have to teach it something
else. The JSON summary should carry the same line the text layout prints,
without color, as `summary.text`, and the internal and main counts their
`internal: 2 packages changed · ...` lines likewise. The counts stay; the
text is one more field, and scripts that want the sentence rather than
the numbers get it too.

### Not native: a `--format=github`

The tempting step is one further: a layout that writes
`$GITHUB_STEP_SUMMARY` and `$GITHUB_OUTPUT` and prints `::warning::`
lines, so that the action is a `go build` and one command. The survey
says no. `golangci-lint` had exactly that format, deprecated it in v1.59
and removed it in v2 in favor of problem matchers; `actionlint` documents
a template for it and recommends matchers or reviewdog instead;
`gotestsum` is the one tool with a `github-actions` format, for grouping
test output in logs. Workflow commands are the runner's interface, they
change (`set-output` and `save-state` were deprecated in 2022), and the
limits around them, 1 MiB per summary and per output, 10 annotations of
each kind per step, are the action's problem to manage, not a diff tool's.
The same goes for posting the comment from the binary: this tool promises
to touch nothing but the module cache, and an HTTP client to GitHub inside
it would be a strange neighbour to `--fsreadonly`.

So the tool grows two revision and JSON features, and the glue remains
glue. The question is what the glue is written in.

### The glue: from bash to a Go program in the module

The script's remaining 165 lines depend on `bash`, `jq`, `curl`, `awk`,
`sed` and `paste`, and on their GNU and BSD variants agreeing. GitHub's
runners have them all; self-hosted runners have whatever they have, and
`jq` missing is the classic failure of composite actions. It is checked by
`shellcheck` and tested by running it on this repository's pull requests.

The action already requires a Go toolchain, and it already builds the
tool from its own checkout. The same `go build` can build the glue: a
second `main` package in the module, `internal/action` say, that the
composite runs after the build. It reads the `INPUT_*` environment,
calls the library once, and writes the summary, the outputs, the
annotations and the comment with `encoding/json` and `net/http`. Nothing
that `jq`, `curl` or `awk` do is hard in Go; what they do badly, quoting,
escaping, pagination and truncation at a paragraph boundary, is where the
script's density comes from. The comment logic is about ninety lines of
Go with a unit test against `httptest`, which the bash version cannot
have.

What it changes in the module:

- **The library exposes a result.** `whatchanged.Run` prints and returns
  an exit code; the action wants the `render.Result` to summarize, render
  as markdown and render as JSON without a second load. A `Diff(opts)
  (*render.Result, error)` next to `Run`, which `Run` itself uses, is the
  whole change. The internal package stays internal.
- **One load instead of two.** The script runs the tool twice for the two
  layouts. In-process it is one type-check and two renders, and the
  warnings come as values rather than `warn:` lines.
- **No new dependencies.** The stdlib covers it. `sethvargo/go-githubactions`
  offers `SetOutput`, `AddStepSummary` and `Warningf` with the escaping
  done, and has no dependencies of its own, but at this size the
  functions are as easily written as imported.
- **`shellcheck` leaves CI**, and `go test ./...` covers the action's
  logic, including the cases the script handles by inspection today: the
  comment that is too long, the fork's read-only token, the first comment
  that waits for something to show, the marker per module directory.

The precedent among Go-authored actions is the Docker action with a Go
`main`, as `gosec` ships: correct, but Linux-only and a registry pull
per run. A composite that builds and runs a Go program is lighter and
runs wherever the tool does, on macOS and Windows runners included.

The alternative worth naming is to keep bash but delegate the comment,
the largest piece, to `marocchino/sticky-pull-request-comment` inside
the composite: it upserts by a `header` marker, has `only_update` for the
first-comment-waits rule and `hide_and_recreate` for outdated comments.
It brings a pinned third-party action into the composite, does not
truncate at 65536, and leaves everything else in bash. It is the smaller
change; the Go program is the one that removes the script.

### Smaller things the script gets wrong or could do better

- **`push` without a tag fails.** The default base on any event but
  `pull_request` is `@latest`, so a repository with no release tags gets a
  red step on its first push to main. `github.event.before`, the previous
  tip, answers "what did this push change" and is always available; a
  notice when there are no tags, and that base, would make the action
  safe to add to `on: push`.
- **The `markdown` and `json` outputs are unbounded.** Outputs are capped
  at 1 MB per job, and GitHub's docs say an arbitrary value should go to
  a file instead. Paths to the two reports under `$RUNNER_TEMP`, as
  `markdown-file` and `json-file`, next to the small outputs, would not
  break a large module.
- **The job summary is unbounded too**, at 1 MiB per step, after which
  the upload fails with an annotation. The comment already truncates at a
  paragraph boundary; the summary should use the same code with a larger
  limit.
- **Inline annotations with `pos`.** The tool knows the file and line of
  every changed declaration. `::warning file=store/client.go,line=12::`
  on each incompatible change would show it in the pull request's Files
  view, where the reviewer is. Ten per step is the cap, which suits a
  "the first ten incompatible changes" rule.
- **Do not fetch tags into a shallow clone.** `fetch_commit` uses
  `--no-tags`, which is right; the README's `fetch-depth: 0` line is the
  documented way to get history and tags, and `fetch-tags: true` with a
  depth is the cheaper form for `@latest` when a project cares.

## In what order

1. **Move a `v1` tag on each release and list on the Marketplace.** A
   release workflow and a README edit; no code.
2. **`@rev...` and `summary.text` in the tool.** Two features with tests
   and golden files, useful on the command line, and the script shrinks
   by forty lines and its most fragile part the day they land.
3. **Rewrite the glue in Go**, with `Diff` exposed from the library, and
   delete the script and the `shellcheck` step. The `push` default, the
   file outputs and the summary limit come with it, since they are
   easier in Go than in the script they would otherwise extend.
4. **Restructure the README section** around the result, the minimal
   workflow and the cookbook, once the pin and the merge-base spelling
   exist to put in it.
5. **Inline annotations**, later, as a `pos`-dependent extra.

## Sources

- golangci-lint-action `action.yml`, `src/install.ts`, `src/version.ts`:
  `install-mode: binary|goinstall|none`, `version`, `version-file`;
  README: "v4.0.0+ requires an explicit `actions/setup-go` installation
  step". <https://github.com/golangci/golangci-lint-action>
- golangci-lint discussion on the removal of `--out-format=github-actions`:
  <https://github.com/golangci/golangci-lint/discussions/5703>
- goreleaser-action `src/goreleaser.ts`: release tarball with
  `checksums.txt` and a sigstore bundle. <https://github.com/goreleaser/goreleaser-action>
- govulncheck-action `action.yml`: `setup-go` inside the composite, then
  `go install golang.org/x/vuln/cmd/govulncheck@latest`.
  <https://github.com/golang/govulncheck-action>
- staticcheck-action `action.yaml`: `install-go`, `version`, `actions/cache`
  on the build cache. <https://github.com/dominikh/staticcheck-action>
- gosec `action.yml`: Docker action pinned by image digest.
  <https://github.com/securego/gosec>
- cargo-semver-checks-action `src/main.ts`: latest release binary, else
  `cargo install`; README: no comment, no destination-branch baseline.
  <https://github.com/obi1kenobi/cargo-semver-checks-action>
- go-apidiff `action.yml`: `go install ...@${{ inputs.version }}`,
  `base-ref` defaulting to `pull_request.base.sha ||
  merge_group.base_sha`, `output` and `semver-type` outputs.
  <https://github.com/joelanford/go-apidiff>
- Trivy's `apidiff.yaml`: `pull_request_target`, checkout of
  `refs/pull/N/merge`, fetch of the base branch by name because `base.sha`
  is stale, upsert of a comment by marker with `actions/github-script`.
  <https://github.com/aquasecurity/trivy/blob/main/.github/workflows/apidiff.yaml>
- kubebuilder pinning `go-apidiff` by SHA with a version comment.
  <https://github.com/kubernetes-sigs/kubebuilder/blob/master/.github/workflows/apidiff.yml>
- marocchino/sticky-pull-request-comment: `header`, `only_update`,
  `hide_and_recreate`, `skip_unchanged`.
  <https://github.com/marocchino/sticky-pull-request-comment>
- GitHub docs, managing custom actions: tag releases, move the major tag,
  do not reference the default branch; publishing on the Marketplace;
  immutable releases and movable tags; workflow commands and the 1 MiB
  summary and command-file caps; output limits, 1 MB per job and 50 MB per
  run; `pull_request_target` and fork tokens. <https://docs.github.com/actions>
- actions/toolkit `docs/action-versioning.md`: "do not reference `main`",
  `git tag -fa v1`. <https://github.com/actions/toolkit>
- actions/runner-images, Ubuntu 24.04: Go 1.24, 1.25 and 1.26, jq, curl and
  the GitHub CLI preinstalled. <https://github.com/actions/runner-images>
- rmacklin/fetch-through-merge-base: iterative `git fetch --deepen` until
  the merge-base is reachable. <https://github.com/rmacklin/fetch-through-merge-base>

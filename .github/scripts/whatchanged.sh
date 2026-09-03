#!/usr/bin/env bash
# The second step of the composite action in action.yml: resolves the two
# revisions, warms the module cache for both sides, runs go-whatchanged and
# turns the result into a job summary, step outputs and annotations.
#
# The action's inputs arrive as INPUT_* variables. PR_BASE_SHA is the tip of
# the base branch of the pull request being built, empty on other events.
# The binary comes from the action's first step.
set -euo pipefail

work="$RUNNER_TEMP/go-whatchanged"
bin="$work/go-whatchanged"
[ -x "$bin" ] || bin="$bin.exe"
json="$work/report.json"
md="$work/report.md"
stderr="$work/stderr.txt"

log() { echo "go-whatchanged: $*"; }

# annotate prints a workflow command that GitHub shows as an annotation:
# annotate warning "message". It goes to stderr, which the runner reads for
# commands too, so that callers inside command substitutions stay clean.
annotate() {
  local level=$1 msg=$2 title
  title=$(printf '%s' "${INPUT_TITLE:-go-whatchanged}" |
    sed -e 's/%/%25/g' -e 's/:/%3A/g' -e 's/,/%2C/g')
  msg=$(printf '%s' "$msg" |
    awk '{gsub(/%/, "%25"); printf "%s%s", (NR > 1 ? "%0A" : ""), $0}')
  echo "::$level title=$title::$msg" >&2
}

# error_text is what to show for a failed run: the tool's error and the
# lines that follow it, such as a suggested command, or the last line of
# stderr $1 when there is no such error.
error_text() {
  local text
  text=$(sed -n '/^go-whatchanged: /,$p' "$1")
  if [ -n "$text" ]; then echo "$text"; else tail -n 1 "$1"; fi
}

is_shallow() { [ "$(git rev-parse --is-shallow-repository)" = true ]; }

# fetch_commit fetches a commit the checkout lacks, keeping a shallow clone
# shallow and a full clone full.
fetch_commit() {
  if is_shallow; then
    git fetch --quiet --no-tags --depth=1 origin "$1"
  else
    git fetch --quiet --no-tags origin "$1"
  fi
}

# resolve_base picks the old side when the base input is empty: on a pull
# request the merge-base of head and the base branch, which is what the pull
# request itself changes, or the tip of the base branch when the history to
# find the merge-base is missing; @latest on any other event.
resolve_base() {
  if [ -z "$PR_BASE_SHA" ]; then
    echo @latest
    return
  fi
  git cat-file -e "$PR_BASE_SHA^{commit}" 2>/dev/null || fetch_commit "$PR_BASE_SHA"
  local mb
  if mb=$(git merge-base "$PR_BASE_SHA" "${head:-HEAD}" 2>/dev/null); then
    echo "$mb"
  else
    annotate warning "not enough history to find the merge-base of ${head:-HEAD} and ${GITHUB_BASE_REF:-the base branch}; diffing against its tip instead. Check out with fetch-depth: 0 for an exact diff."
    echo "$PR_BASE_SHA"
  fi
}

# module_root is the nearest go.mod at or above the working directory: the
# module go-whatchanged diffs.
module_root() {
  local d
  d=$(pwd -P)
  while [ ! -f "$d/go.mod" ]; do
    [ "$d" = / ] && return 1
    d=$(dirname "$d")
  done
  echo "$d"
}

# download_deps_of downloads the dependencies pinned by the go.mod of commit
# $1 into the module cache, from a copy in the temp directory so that
# nothing in the checkout is written. Returns non-zero when the commit has
# no go.mod for this module or the download fails.
download_deps_of() {
  local commit=$1 root prefix dir
  root=$(module_root) || return 1
  prefix=$(git -C "$root" rev-parse --show-prefix 2>/dev/null) || return 1
  dir="$work/deps/$commit"
  mkdir -p "$dir"
  git show "$commit:${prefix}go.mod" >"$dir/go.mod" 2>/dev/null || return 1
  git show "$commit:${prefix}go.sum" >"$dir/go.sum" 2>/dev/null || rm -f "$dir/go.sum"
  (cd "$dir" && GOWORK=off go mod download)
}

# warm_cache downloads the dependencies of both sides into the module cache,
# since go-whatchanged never fetches anything: with go mod download in the
# checkout for the head side, and from the base revision's go.mod for the
# base side. A base of @latest is resolved by the tool, so its modules are
# fetched on demand in run_json instead. Failures here are warnings only;
# the diff decides.
warm_cache() {
  local root commit
  root=$(module_root) || return 0
  (cd "$root" && go mod download) || annotate warning "go mod download failed in $root"
  commit=$(git rev-parse --verify --quiet "$base^{commit}") || return 0
  warmed=$commit
  download_deps_of "$commit" || annotate warning "go mod download failed for the go.mod of $base"
}

# run_json writes the JSON report. When the tool stops on a module missing
# from the cache, one the warming above could not know about, it downloads
# the dependencies of the revision the error names (the tag @latest
# resolved to, say), or just that module when the revision is not one, and
# tries again, up to a limit. Leaves the exit code in rc.
run_json() {
  local mod rev commit
  for _ in $(seq 1 25); do
    rc=0
    "$bin" "${flags[@]}" --format=json "${revs[@]}" >"$json" 2>"$stderr" || rc=$?
    [ "$rc" -eq 2 ] || return 0
    mod=$(sed -n 's/.*module \([^ ]*\) not in module cache.*/\1/p' "$stderr" | head -n 1)
    [ -n "$mod" ] || return 0
    rev=$(sed -n 's/^go-whatchanged: \([^:]*\): .*not in module cache.*/\1/p' "$stderr" | head -n 1)
    commit=$(git rev-parse --verify --quiet "$rev^{commit}" 2>/dev/null) || commit=
    if [ -n "$commit" ] && [ "$commit" != "$warmed" ]; then
      log "downloading the dependencies of $rev"
      warmed=$commit
      download_deps_of "$commit" || log "go mod download failed for the go.mod of $rev"
    else
      log "downloading $mod"
      (cd "$work" && go mod download "$mod") || return 0
    fi
  done
}

warmed=
head=$INPUT_HEAD
base=$INPUT_BASE
[ -n "$base" ] || base=$(resolve_base)
revs=("$base")
[ -z "$head" ] || revs+=("$head")

flags=(--filter "$INPUT_FILTER" --signatures "$INPUT_SIGNATURES")
[ -z "$INPUT_PKG" ] || flags+=(--pkg "$(printf '%s' "$INPUT_PKG" | tr '\n' ,)")
[ -z "$INPUT_EXCLUDE" ] || flags+=(--exclude "$(printf '%s' "$INPUT_EXCLUDE" | tr '\n' ,)")
[ "$INPUT_BREAKING" != true ] || flags+=(--filter breaking)
# shellcheck disable=SC2153 # INPUT_POS is an input, not a typo of INPUT_GOOS
[ "$INPUT_POS" != true ] || flags+=(--pos)
[ "$INPUT_STRICT" != true ] || flags+=(--strict)
# The build target is taken from the environment, as by the go command.
[ -z "$INPUT_GOOS" ] || export GOOS="$INPUT_GOOS"
[ -z "$INPUT_GOARCH" ] || export GOARCH="$INPUT_GOARCH"

log "comparing $base with ${head:-the working tree}"
warm_cache

rc=0
run_json
if [ "$rc" -ne 0 ] && [ "$rc" -ne 1 ]; then
  cat "$stderr" >&2
  annotate error "$(error_text "$stderr")"
  exit "$rc"
fi

# The markdown pass also carries --exit-fail, so the exit code is the tool's.
fail=()
[ -z "$INPUT_FAIL_ON" ] || fail=(--exit-fail "$INPUT_FAIL_ON")
rc=0
"$bin" "${flags[@]}" ${fail[@]+"${fail[@]}"} --format=markdown "${revs[@]}" >"$md" 2>"$stderr" || rc=$?
# Type-check problems come out as "warn: ..." lines.
while IFS= read -r line; do
  case $line in
    "warn: "*) annotate warning "${line#warn: }" ;;
    *) echo "$line" >&2 ;;
  esac
done <"$stderr"
if [ "$rc" -eq 2 ]; then
  annotate error "$(error_text "$stderr")"
  exit "$rc"
fi

cat "$md"
if [ "$INPUT_SUMMARY" = true ]; then
  {
    [ -z "$INPUT_TITLE" ] || printf '### %s\n\n' "$INPUT_TITLE"
    cat "$md"
  } >>"$GITHUB_STEP_SUMMARY"
fi

# The summary lines are everything outside the fenced blocks that is not a
# package heading, minus the markdown emphasis: the public API's line and,
# when internal packages changed, theirs.
summary=$(awk '/^```/ {fence = !fence; next} fence || /^\*\*/ || /^$/ {next} {print}' "$md" |
  sed -e 's/\*\*//g' -e 's/^_\(.*\)_$/\1/')

delim="go-whatchanged-$RANDOM$RANDOM"
{
  for key in release packages_changed incompatible compatible; do
    echo "${key//_/-}=$(jq -r ".summary.$key" "$json")"
  done
  echo "base=$(jq -r .base "$json")"
  echo "head=$(jq -r .head "$json")"
  echo "base-version=$(jq -r '.base_version // ""' "$json")"
  echo "next-version=$(jq -r '.next_version // ""' "$json")"
  printf 'summary<<%s\n%s\n%s\n' "$delim" "$summary" "$delim"
  printf 'markdown<<%s\n' "$delim"
  cat "$md"
  echo "$delim"
  printf 'json<<%s\n' "$delim"
  cat "$json"
  echo "$delim"
} >>"$GITHUB_OUTPUT"

case $rc in
  0 | 1)
    if [ "$(jq -r .summary.incompatible "$json")" -eq 0 ]; then
      annotate notice "$summary"
    else
      annotate warning "$summary"
    fi
    ;;
  *)
    annotate error "$summary
The step fails because fail-on is set to $INPUT_FAIL_ON."
    exit "$rc"
    ;;
esac

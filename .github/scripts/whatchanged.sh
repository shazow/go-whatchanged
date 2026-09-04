#!/usr/bin/env bash
# The second step of the composite action in action.yml: resolves the two
# revisions, runs go-whatchanged and turns the result into a job summary,
# step outputs, annotations and a pull request comment.
#
# The action's inputs arrive as INPUT_* variables. PR_BASE_SHA is the tip of
# the base branch of the pull request being built, PR_NUMBER its number and
# PR_HEAD_REPO the repository its head branch lives in, all empty on other
# events; GH_TOKEN is the token for the comment. The binary comes from the
# action's first step.
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
# find the merge-base is missing; @latest on any other event. Sides are
# named as go-whatchanged names them, @rev for a revision of the checkout.
resolve_base() {
  if [ -z "$PR_BASE_SHA" ]; then
    echo @latest
    return
  fi
  git cat-file -e "$PR_BASE_SHA^{commit}" 2>/dev/null || fetch_commit "$PR_BASE_SHA"
  local mb h=${head#@}
  if mb=$(git merge-base "$PR_BASE_SHA" "${h:-HEAD}" 2>/dev/null); then
    echo "@$mb"
  else
    annotate warning "not enough history to find the merge-base of ${h:-HEAD} and ${GITHUB_BASE_REF:-the base branch}; diffing against its tip instead. Check out with fetch-depth: 0 for an exact diff."
    echo "@$PR_BASE_SHA"
  fi
}

head=$INPUT_HEAD
base=$INPUT_BASE
[ -n "$base" ] || base=$(resolve_base)
revs=("$base")
[ -z "$head" ] || revs+=("$head")

flags=(--filter "$INPUT_FILTER")
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

rc=0
"$bin" "${flags[@]}" --format=json "${revs[@]}" >"$json" 2>"$stderr" || rc=$?
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

# The summary lines are everything outside the fenced blocks that is not a
# package heading, minus the markdown emphasis: the public API's line and,
# when internal or main packages changed, theirs.
summary=$(awk '/^```/ {fence = !fence; next} fence || /^### / || /^$/ {next} {print}' "$md" |
  sed -e 's/\*\*//g' -e 's/^_\(.*\)_$/\1/')

# The glyph stands for the release the public API changes call for: green
# for none, yellow for minor, red for major.
case $(jq -r .summary.release "$json") in
  major) glyph="🔴" ;;
  minor) glyph="🟡" ;;
  *) glyph="🟢" ;;
esac
run_url="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"

# short abbreviates a full commit hash, leaving tags and refs alone.
short() {
  if [[ $1 =~ ^[0-9a-f]{40}$ ]]; then echo "${1:0:7}"; else echo "$1"; fi
}
compared="Compared <code>$(short "$(jq -r .base "$json")")</code> with <code>$(short "$(jq -r .head "$json")")</code> · <a href=\"$run_url\">job summary</a>"

echo "::group::report"
cat "$md"
echo "::endgroup::"
if [ "$INPUT_SUMMARY" = true ]; then
  {
    # A level above the report's package headings.
    [ -z "$INPUT_TITLE" ] || printf '## %s %s\n\n' "$glyph" "$INPUT_TITLE"
    cat "$md"
    printf '\n<sub>%s</sub>\n' "$compared"
  } >>"$GITHUB_STEP_SUMMARY"
fi

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

# api calls the GitHub REST API: api METHOD PATH [FILE], with FILE as the
# JSON request body. Leaves the status code in http and in $work/http, for
# callers in a subshell, and the response in $work/response.json; returns
# non-zero unless the status is 2xx.
api() {
  local method=$1 path=$2 data=()
  [ -z "${3:-}" ] || data=(-H "Content-Type: application/json" --data-binary "@$3")
  http=$(curl -sS -o "$work/response.json" -w '%{http_code}' -X "$method" \
    -H "Authorization: Bearer $GH_TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    ${data[@]+"${data[@]}"} \
    "$GITHUB_API_URL/$path") || http=000
  echo "$http" >"$work/http"
  [ "${http:0:1}" = 2 ]
}

# find_comment prints the id of the pull request comment carrying the
# marker $1, if any: the one an earlier run of this action left, to update
# in place rather than add another.
find_comment() {
  local page n
  for page in $(seq 1 30); do
    api GET "repos/$GITHUB_REPOSITORY/issues/$PR_NUMBER/comments?per_page=100&page=$page" || return 1
    jq -r --arg m "$1" 'map(select(.body | startswith($m))) | .[0].id // empty' "$work/response.json"
    n=$(jq length "$work/response.json")
    [ "$n" -eq 100 ] || return 0
  done
}

# comment_body writes the pull request comment to $1: the marker, then the
# report folded into a details block whose summary line is the verdict,
# with a footer that names the sides, links the job summary and credits
# the tool. Comments hold 65536 characters; a longer report is cut at a
# paragraph boundary and points at the job summary, which has no limit.
comment_body() {
  local out=$1 marker=$2 limit=65536 report=$work/comment-report.md room footer
  footer="<sub>$compared · powered by <a href=\"https://github.com/shazow/go-whatchanged\">go-whatchanged</a></sub>"
  {
    echo "$marker"
    echo "<details>"
    printf '<summary>%s%s%s</summary>\n\n' "$glyph " "${INPUT_TITLE:+<b>$INPUT_TITLE</b>: }" \
      "$(printf '%s\n' "$summary" | paste -sd '|' | sed 's/|/ · /g')"
  } >"$out"
  room=$((limit - $(wc -c <"$out") - $(printf '%s' "$footer" | wc -c) - 200))
  if [ "$(wc -c <"$md")" -le "$room" ]; then
    cp "$md" "$report"
  else
    head -c "$room" "$md" | awk 'BEGIN {RS = ""; ORS = "\n\n"} {para[NR] = $0} END {for (i = 1; i < NR; i++) print para[i]}' >"$report"
    [ $(($(grep -c '^```' "$report") % 2)) -eq 0 ] || echo '```' >>"$report"
    printf '\n_The report is longer than a comment can hold; the [job summary](%s) has all of it._\n' "$run_url" >>"$report"
  fi
  {
    cat "$report"
    printf '\n%s\n</details>\n' "$footer"
  } >>"$out"
}

# upsert_comment posts the report as a pull request comment, or updates
# the one an earlier run left. A first comment waits until there is
# something to show, so a pull request that never touches the API gets
# none; once it exists it follows every push.
upsert_comment() {
  local dir marker id body=$work/comment.md request=$work/request.json
  # The marker names the module's directory, so that one workflow can diff
  # several modules into comments of their own.
  dir=$(pwd -P | sed "s#^$GITHUB_WORKSPACE/\{0,1\}##")
  marker="<!-- go-whatchanged: ${dir:-.} -->"
  if ! id=$(find_comment "$marker"); then
    annotate warning "could not list the pull request's comments (HTTP $(cat "$work/http")). The comment needs pull-requests: write under the workflow's permissions; set comment: false for the job summary alone."
    return
  fi
  if [ -z "$id" ] && [ "$(jq '.packages | length' "$json")" -eq 0 ]; then
    log "nothing to show; no comment"
    return
  fi
  comment_body "$body" "$marker"
  jq -n --rawfile body "$body" '{body: $body}' >"$request"
  if [ -n "$id" ]; then
    api PATCH "repos/$GITHUB_REPOSITORY/issues/comments/$id" "$request" && log "updated comment $id" && return
  else
    api POST "repos/$GITHUB_REPOSITORY/issues/$PR_NUMBER/comments" "$request" && log "posted comment $(jq .id "$work/response.json")" && return
  fi
  # A pull request from a fork gets a read-only token, so its comment is
  # never posted: a notice, since there is nothing the workflow can change.
  if [ -n "$PR_HEAD_REPO" ] && [ "$PR_HEAD_REPO" != "$GITHUB_REPOSITORY" ]; then
    annotate notice "no comment on a pull request from a fork, whose token cannot post one (HTTP $http); the job summary has the diff."
    return
  fi
  annotate warning "could not post the pull request comment (HTTP $http). The comment needs pull-requests: write under the workflow's permissions; set comment: false for the job summary alone."
}

if [ "$INPUT_COMMENT" = true ]; then
  if [ -n "$PR_NUMBER" ]; then
    upsert_comment
  else
    log "not a pull request; no comment"
  fi
fi

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

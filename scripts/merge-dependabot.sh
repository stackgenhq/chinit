#!/usr/bin/env bash
#
# merge-dependabot.sh — Squash-merge all open Dependabot PRs into the default branch.
#
# Usage:
#   ./scripts/merge-dependabot.sh                        # uses current repo (gh detects from git remote)
#   ./scripts/merge-dependabot.sh appcd-dev/genie        # explicit repo
#   ./scripts/merge-dependabot.sh appcd-dev/genie --dry-run  # preview without merging
#
# Requirements: gh CLI (https://cli.github.com) authenticated with sufficient permissions, and jq.

set -euo pipefail

# ── Colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

# ── Helpers ──────────────────────────────────────────────────────────────────
info()    { echo -e "${CYAN}ℹ ${RESET}$*"; }
success() { echo -e "${GREEN}✅ ${RESET}$*"; }
warn()    { echo -e "${YELLOW}⚠️  ${RESET}$*"; }
fail()    { echo -e "${RED}❌ ${RESET}$*"; }

usage() {
  echo "Usage: $0 [OWNER/REPO] [--dry-run] [--admin]"
  echo
  echo "  OWNER/REPO   GitHub repository (e.g. appcd-dev/genie)."
  echo "               If omitted, inferred from the current git remote."
  echo "  --dry-run    List Dependabot PRs without merging."
  echo "  --admin      Use admin privileges for merging (bypasses branch protection)."
  exit 1
}

# ── Parse args ───────────────────────────────────────────────────────────────
REPO=""
DRY_RUN=false
ADMIN_FLAG=false

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    --admin) ADMIN_FLAG=true ;;
    --help|-h) usage ;;
    *)
      if [[ -z "$REPO" ]]; then
        REPO="$arg"
      else
        echo "Unknown argument: $arg"
        usage
      fi
      ;;
  esac
done

# ── Pre-flight checks ───────────────────────────────────────────────────────
if ! command -v gh &>/dev/null; then
  echo -e "${RED}Error:${RESET} gh CLI is not installed. Install it from https://cli.github.com"
  exit 1
fi

if ! command -v jq &>/dev/null; then
  echo -e "${RED}Error:${RESET} jq is not installed. Please install it."
  exit 1
fi

if ! gh auth status &>/dev/null; then
  echo -e "${RED}Error:${RESET} gh CLI is not authenticated. Run 'gh auth login' first."
  exit 1
fi

if [[ -z "$REPO" ]] && ! git rev-parse --is-inside-work-tree &>/dev/null; then
  echo -e "${RED}Error:${RESET} Not inside a git repository. Please specify OWNER/REPO or run inside a git repo."
  usage
fi

# Build the --repo flag (empty string means gh will auto-detect).
REPO_FLAG=()
if [[ -n "$REPO" ]]; then
  REPO_FLAG=(--repo "$REPO")
fi

DISPLAY_REPO="${REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner "${REPO_FLAG[@]}" 2>/dev/null || echo 'current repo')}"

# ── Fetch open Dependabot PRs ───────────────────────────────────────────────
info "Fetching open Dependabot PRs for ${BOLD}${DISPLAY_REPO}${RESET} …"

PR_JSON=$(gh pr list "${REPO_FLAG[@]}" \
  --state open \
  --json number,title,mergeable,mergeStateStatus,author \
  --limit 100 | jq '[.[] | select(.author.login == "dependabot[bot]" or .author.login == "app/dependabot")]')

PR_COUNT=$(echo "$PR_JSON" | jq 'length')

if [[ "$PR_COUNT" -eq 0 ]]; then
  success "No open Dependabot PRs found. Nothing to do!"
  exit 0
fi

echo -e "${BOLD}Found ${PR_COUNT} open Dependabot PR(s):${RESET}"
echo "$PR_JSON" | jq -r '.[] | "  #\(.number)  \(.mergeable | if . == "MERGEABLE" then "✓" else "✗" end)  \(.title)"'
echo

if $DRY_RUN; then
  warn "Dry-run mode — no PRs will be merged."
  exit 0
fi

# ── Merge each PR ────────────────────────────────────────────────────────────
MERGED=0
FAILED=0
CONFLICT=0
FAILED_PRS=()

MERGE_ARGS=(--squash)
if $ADMIN_FLAG; then MERGE_ARGS+=(--admin); fi

# Sort by PR number (oldest first) so dependent bumps resolve in order.
# Single jq pass to extract fields and read line by line.
while IFS="|" read -r pr mergeable status title; do
  echo -ne "  ${BOLD}#${pr}${RESET} ${title} … "

  if [[ "$mergeable" == "UNKNOWN" ]]; then
    echo -e "${YELLOW}skipped (mergeability UNKNOWN — GitHub is still calculating)${RESET}"
    CONFLICT=$((CONFLICT + 1))
    FAILED_PRS+=("#${pr} (unknown state)")
    continue
  elif [[ "$mergeable" != "MERGEABLE" && "$status" != "CLEAN" && "$status" != "BLOCKED" ]]; then
    echo -e "${YELLOW}skipped (not mergeable — likely has conflicts)${RESET}"
    CONFLICT=$((CONFLICT + 1))
    FAILED_PRS+=("#${pr} (conflict)")
    continue
  fi

  # Approve the PR first
  if [[ "$(gh pr view "$pr" "${REPO_FLAG[@]}" --json author --jq .author.login)" == "dependabot[bot]" ]]; then
    gh pr review "$pr" "${REPO_FLAG[@]}" --approve >/dev/null 2>&1 || true
  fi

  # Run the merge command and capture std error
  if output=$(gh pr merge "$pr" "${REPO_FLAG[@]}" "${MERGE_ARGS[@]}" 2>&1); then
    echo -e "${GREEN}merged${RESET}"
    MERGED=$((MERGED + 1))
    sleep 2
  else
    echo -e "${RED}failed${RESET}"
    echo -e "      ${YELLOW}Reason:${RESET} | $output"
    FAILED=$((FAILED + 1))
    FAILED_PRS+=("#${pr}")
  fi
done < <(echo "$PR_JSON" | jq -r 'sort_by(.number) | .[] | "\(.number)|\(.mergeable)|\(.mergeStateStatus)|\(.title)"')

# ── Summary ──────────────────────────────────────────────────────────────────
echo
echo -e "${BOLD}── Summary ──${RESET}"
success "Merged:    ${MERGED}"
if [[ "$CONFLICT" -gt 0 ]]; then
  warn "Conflicts: ${CONFLICT}"
fi
if [[ "$FAILED" -gt 0 ]]; then
  fail "Failed:    ${FAILED}"
fi

if [[ ${#FAILED_PRS[@]} -gt 0 ]]; then
  echo
  warn "The following PRs were not merged:"
  for p in "${FAILED_PRS[@]}"; do
    echo "    • $p"
  done
  echo
  info "Dependabot will usually auto-rebase conflicting PRs within a few minutes."
  info "You can also close them and let Dependabot recreate fresh PRs."
fi

exit $FAILED

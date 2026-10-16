#!/bin/sh
# study.sh: the sign-in study as one run.
#
# For each mode and each site it opens the window (you sign in at
# http://127.0.0.1:7788/ and click "Save and close"), then checks three
# restarts. At the end it runs memory and attack in both modes. Results
# accumulate in results/findings.md and results/attack.md.
#
# Rerunnable: a site whose profile already exists skips the sign-in step
# (REDO=1 forces it), and SITES / MODES narrow the run.
#
#   sh study.sh                          # everything, four sites, both modes
#   SITES="github amazon" sh study.sh    # some sites
#   MODES=headless sh study.sh           # one mode
#   REDO=1 SITES=linkedin sh study.sh    # sign in to LinkedIn again
#   MS_PERSONAL=1 sh study.sh            # a personal Microsoft account (login.live.com)
#   SKIP_MEMORY=1 SKIP_ATTACK=1 sh study.sh
set -eu
cd "$(dirname "$0")"

: "${SITES:=github linkedin amazon microsoft}"
: "${MODES:=headless xvfb}"
: "${RESTARTS:=3}"
: "${REDO:=}"
: "${MS_PERSONAL:=}"
: "${SKIP_MEMORY:=}"
: "${SKIP_ATTACK:=}"
# The dev Mac's shell exports a GOROOT that does not match the go on PATH.
: "${GOROOT:=/opt/homebrew/opt/go/libexec}"
export GOROOT

login_url() {
  case "$1" in
    github) echo https://github.com/login ;;
    linkedin) echo https://www.linkedin.com/login ;;
    amazon) echo https://www.amazon.com/gp/sign-in.html ;;
    microsoft)
      if [ -n "$MS_PERSONAL" ]; then echo https://login.live.com/; else echo https://login.microsoftonline.com/; fi ;;
    *) echo "study.sh: unknown site '$1' (github, linkedin, amazon, microsoft)" >&2; exit 2 ;;
  esac
}

check_url() {
  case "$1" in
    github) echo https://github.com/settings/profile ;;
    linkedin) echo https://www.linkedin.com/mypreferences/d/categories/account ;;
    amazon) echo https://www.amazon.com/gp/css/order-history ;;
    microsoft)
      if [ -n "$MS_PERSONAL" ]; then echo https://account.microsoft.com/; else echo https://myaccount.microsoft.com/; fi ;;
  esac
}

for site in $SITES; do login_url "$site" >/dev/null; done

if [ ! -x bin/spike ]; then
  echo "study.sh: building bin/spike"
  go build -o bin/ ./cmd/...
fi
if ! docker image inspect kivali-spike-browser:dev >/dev/null 2>&1; then
  echo "study.sh: building the browser image (once)"
  docker build -t kivali-spike-browser:dev .
fi

total=0
for mode in $MODES; do for site in $SITES; do total=$((total + 1)); done; done
n=0
for mode in $MODES; do
  for site in $SITES; do
    n=$((n + 1))
    session="$site-$mode"
    echo
    echo "=== $n of $total: $site, $mode (session $session) ==="
    if [ -f "profiles/$session.tar.zst" ] && [ -z "$REDO" ]; then
      echo "profile exists; skipping the sign-in step (REDO=1 to sign in again)"
    else
      echo "Sign in at http://127.0.0.1:7788/ then click \"Save and close\" in that page."
      bin/spike window --session "$session" --mode "$mode" --url "$(login_url "$site")"
    fi
    echo "checking $RESTARTS restarts against $(check_url "$site")"
    bin/spike check --session "$session" --mode "$mode" --restarts "$RESTARTS" --url "$(check_url "$site")"
  done
done

if [ -z "$SKIP_MEMORY" ]; then
  for mode in $MODES; do
    echo
    echo "=== memory: 3 browsers x 4 tabs, $mode ==="
    bin/spike memory --browsers 3 --tabs 4 --mode "$mode"
  done
fi
if [ -z "$SKIP_ATTACK" ]; then
  for mode in $MODES; do
    echo
    echo "=== attack set, $mode ==="
    bin/spike attack --mode "$mode"
  done
fi

echo
echo "Done. Read results/findings.md and results/attack.md; screenshots are in results/."

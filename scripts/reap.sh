#!/bin/bash
# reap.sh [--kill]: find (and with --kill, stop) leftovers of finished agents on the VM.
# Dry run by default. Only these signatures are touched, and only when older than
# REAP_MIN minutes (default 30) and not part of a running merge-gate or perf-run:
#   1. wait loops:  bash -c "while|until ... pgrep -f ... sleep"  (a pgrep -f pattern
#      matches its own command line, so such a loop never ends)
#   2. test servers: /tmp/staskd-bin*/staskd and /tmp/Test*/ staskd nodes
#   3. stale worktrees whose branch is already merged into main (reported only)
# It never touches docker, the data directories or any other process.
set -u
KILL=0; [ "${1:-}" = "--kill" ] && KILL=1
MIN=${REAP_MIN:-30}
protected() { # pids that belong to a running gate/perf run (ancestors included)
  for r in $(pgrep -f 'scripts/merge-gate.sh|scripts/perf-run.sh' 2>/dev/null); do
    echo "$r"; pgrep -P "$r" 2>/dev/null
  done
  pgrep -f 'go test' 2>/dev/null
}
P=$(protected | sort -u | tr '\n' ' ')
in_protected() { # pid or any ancestor in $P
  local p=$1
  while [ "$p" -gt 1 ] 2>/dev/null; do
    case " $P " in *" $p "*) return 0;; esac
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ')
    [ -z "$p" ] && break
  done
  return 1
}
n=0
while read -r pid et args; do
  [ "$pid" = "$$" ] && continue
  [ $((et/60)) -lt "$MIN" ] && continue
  case "$args" in
    "bash -c "*pgrep\ -f*sleep*|"bash -c "*until*sleep*|"bash -c "*while*pgrep*) kind=wait-loop;;
    /tmp/staskd-bin*/staskd*|*/tmp/Test*/*staskd*) kind=test-server;;
    *) continue;;
  esac
  in_protected "$pid" && continue
  n=$((n+1)); printf '%s %s age=%dm %s\n' "$kind" "$pid" $((et/60)) "$(echo "$args" | cut -c1-90)"
  [ $KILL = 1 ] && kill "$pid" 2>/dev/null
done < <(ps -eo pid=,etimes=,args= --sort=etimes)
echo "reap: $n leftover(s)$([ $KILL = 1 ] && echo ' stopped' || echo ' (dry run; use --kill)')"
cd "$(dirname "$0")/.." 2>/dev/null && for w in $(git worktree list --porcelain | awk '/^worktree /{print $2}' | grep '/wt/'); do
  b=$(git -C "$w" rev-parse --abbrev-ref HEAD 2>/dev/null)
  if [ -n "$b" ] && [ "$b" != main ] && [ -z "$(git -C "$w" status --porcelain 2>/dev/null)" ] && git merge-base --is-ancestor "$b" main 2>/dev/null; then echo "merged worktree: $w ($b): remove it with git worktree remove + git branch -d"; fi
done

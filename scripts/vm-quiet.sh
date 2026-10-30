#!/bin/bash
# vm-quiet: report stray long-running helper processes that disturb tests and
# measurements. READ-ONLY: it prints, it never kills anything. The orchestrator
# decides what to ask the user to clear.
#   scripts/vm-quiet.sh            exit 0 = quiet, exit 3 = suspects found
#   MIN_AGE=600 scripts/vm-quiet.sh   age threshold in seconds (default 600)
MIN_AGE=${MIN_AGE:-600}
SELF=$$
found=0
echo "== load: $(cut -d' ' -f1-3 /proc/loadavg) on $(nproc) CPUs"

# list "pid etimes user args" for all processes except this script and its children
list() { ps -eo pid=,ppid=,etimes=,user=,args= | awk -v me=$SELF '$1!=me && $2!=me'; }

echo; echo "== staskd older than ${MIN_AGE}s (stale servers keep ports and disk busy)"
list | awk -v m=$MIN_AGE '$3>=m && $5 ~ /staskd/ && $5 !~ /vm-quiet/ {print "  " $0; n++} END{exit n>0}' || found=1

echo; echo "== watcher loops (bash/sh loops with pgrep -f / sleep: they match themselves and never exit)"
list | awk -v m=60 '$3>=m && $5 ~ /(bash|sh|zsh)$/ && ($0 ~ /pgrep -f/ || $0 ~ /while .*(sleep|pgrep)/ || $0 ~ /until .*(sleep|pgrep)/) && $0 !~ /vm-quiet/ {print "  " $0; n++} END{exit n>0}' || found=1

echo; echo "== orphaned test binaries / go test older than ${MIN_AGE}s"
list | awk -v m=$MIN_AGE '$3>=m && ($5 ~ /\.test( |$)/ || $5 ~ /go test/) {print "  " $0; n++} END{exit n>0}' || found=1

echo; echo "== other heavy tools older than ${MIN_AGE}s (bench / chaos / docker postgres)"
list | awk -v m=$MIN_AGE '$3>=m && $5 ~ /(simbench|simfresh|staskbench|staskmix|staskbulk|staskchaos|staskstress|aggbench|pgdiff|claimcheck)/ {print "  " $0; n++} END{exit n>0}' || found=1

echo; echo "== port lock files held by live test processes ($([ -d /tmp/stask-test-ports ] && ls /tmp/stask-test-ports | wc -l || echo 0) files in /tmp/stask-test-ports)"
if command -v fuser >/dev/null && [ -d /tmp/stask-test-ports ]; then
  for f in /tmp/stask-test-ports/*.lock; do [ -e "$f" ] && fuser "$f" >/dev/null 2>&1 && echo "  held: $f"; done
fi
echo; df -h / | tail -1 | awk '{print "== disk free: " $4 " (stop below 3G)"}'
if [ $found = 0 ]; then echo "QUIET"; exit 0; fi
echo "SUSPECTS FOUND (nothing was killed; ask the user/owner before clearing)"; exit 3

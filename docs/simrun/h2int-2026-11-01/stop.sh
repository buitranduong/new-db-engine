#!/bin/bash
cd ~/h2run; [ -f pid ] && { grep -E "VmRSS|VmHWM" /proc/$(cat pid)/status > ${1:-/dev/null} 2>/dev/null; kill $(cat pid); for i in $(seq 60); do kill -0 $(cat pid) 2>/dev/null || break; sleep 1; done; kill -9 $(cat pid) 2>/dev/null; rm -f pid; }
rm -rf work

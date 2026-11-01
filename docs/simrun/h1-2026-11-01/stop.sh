#!/bin/bash
cd ~/h1run; [ -f pid ] && { kill $(cat pid); for i in $(seq 60); do kill -0 $(cat pid) 2>/dev/null || break; sleep 1; done; kill -9 $(cat pid) 2>/dev/null; rm -f pid; }
rm -rf work

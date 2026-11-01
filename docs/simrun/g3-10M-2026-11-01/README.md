# G3 10M run, 2026-11-01 (second attempt, snapshot reuse)

Harness commit 6afb539 (engine identical to main d017845). N = 10,000,000 sims (43,000,200 rows). T=4 P=4 R=2, 120 s measured after 30 s warm-up.
Labels are the harness' own: hyb-* DIRTY (load1 at start 1.5-1.8 > 1.0), off-* and pg-* INVALID (r2_page below the 30-sample minimum; PostgreSQL also hit 53100 `/dev/shm` 64 MB limit in Docker and was NOT tuned). `equality` did not run (disk). Single runs, no variance known. Interpretation: docs/14 appendix "G3 10M".

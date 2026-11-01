# G3 measurement of the vertical slice (N = 10000000 sims)

Measurements of THIS run on THIS machine at the stated scale and load; no performance claim, no extrapolation. A run labelled DIRTY or INVALID is not a G3 number.

- started 2026-11-01T04:20:00Z, ended 2026-11-01T05:52:44Z; machine load1 at start 2.14, at end 6.33; free disk 20 GB at start, 5 GB at end
- free space the chosen N needs (planning estimate, 0.30 GB per 1M sims for STASK data+log and 0.20 GB per 1M for the PostgreSQL 16 reference; the measured sizes are in the table below): STASK alone 6.0 GB, STASK + PostgreSQL 16 reference (equality run) 12.0 GB, plus the 3 GB floor
- configuration: measured window 120 s after a 30 s warm-up (not recorded, no import), harness `-t 4 -p 4 -r 2`, read-your-writes probes 0, import every 5 s; batched = 5000 rows per transaction with 20ms pause; single = the whole 1% in ONE transaction (worst case); `staskd -scrub-interval 0` in every STASK run, sims WITH (columnar = on) in every STASK run (only `-hybrid` differs)
- configs asked for: state hyb-none hyb-batched hyb-single off-none off-batched off-single pg-none pg-batched pg-single equality

## 1. Runs, machine state, oracles

| run | label | rc | oracles | import shape | load1 start / max / end (harness) | perf-run start / peak | wall s (restore s, ready s) | pristine start | data dir MB | SUT RSS |
|---|---|---|---|---|---|---|---|---|---|---|
| hyb-none | **DIRTY** | 0 | all held (19 oracles) | none | 1.83 / 8.39 / 8.33 (load1 at start 1.83 > 1.0) | 1.00 / 8.55 | 352 (9, 124) | identical | 3092 | VmHWM:	17988340 kB; VmRSS:	13742836 kB |
| hyb-batched | **DIRTY** | 0 | all held (19 oracles) | batched (5000 rows per transaction, 20 ms pause) | 1.53 / 8.19 / 7.69 (load1 at start 1.53 > 1.0) | 0.87 / 8.19 | 438 (9, 131) | identical | 3156 | VmHWM:	18197012 kB; VmRSS:	14277964 kB |
| hyb-single | **DIRTY** | 0 | all held (19 oracles) | single (1% in ONE transaction, worst case) | 1.54 / 8.40 / 7.78 (load1 at start 1.54 > 1.0) | 0.99 / 8.40 | 427 (9, 125) | identical | 3156 | VmHWM:	17986388 kB; VmRSS:	14566032 kB |
| off-none | **INVALID** | 3 | all held (19 oracles) | none | 1.41 / 8.54 / 8.54 (load1 at start 1.41 > 1.0) | 0.93 / 8.54 | 433 (8, 114) | identical | 3092 | VmHWM:	16751788 kB; VmRSS:	12568148 kB |
| off-batched | **INVALID** | 3 | all held (19 oracles) | batched (5000 rows per transaction, 20 ms pause) | 2.10 / 8.13 / 8.11 (load1 at start 2.10 > 1.0) | 0.94 / 8.13 | 437 (8, 112) | identical | 3156 | VmHWM:	16737628 kB; VmRSS:	13088964 kB |
| off-single | **INVALID** | 3 | all held (19 oracles) | single (1% in ONE transaction, worst case) | 2.09 / 8.02 / 7.65 (load1 at start 2.09 > 1.0) | 0.90 / 8.02 | 447 (8, 112) | identical | 3092 | VmHWM:	17517620 kB; VmRSS:	13258588 kB |
| pg-none | **INVALID** | 3 | all held (19 oracles) | none | 1.08 / 14.11 / 13.51 (load1 at start 1.08 > 1.0) | 0.90 / 14.11 | 257 (-, -) | identical | 7113 | - |
| pg-batched | **INVALID** | 3 | all held (19 oracles) | batched (5000 rows per transaction, 20 ms pause) | 0.99 / 14.04 / 14.04 | 0.99 / 14.04 | 306 (-, -) | identical | 7352 | - |
| pg-single | **INVALID** | 3 | all held (19 oracles) | single (1% in ONE transaction, worst case) | 1.00 / 12.87 / 11.78 | 1.00 / 12.87 | 312 (-, -) | identical | 7382 | - |
| equality | **INVALID** | stopped: disk | - | - | - | ? / ? |  (-, -) | ? | - | - |

rc: 0 oracles held, 1 an oracle was violated, 2 setup error, 3-5 perf-run.sh (not quiet / INVALID). The equality run is not a performance run (shorter, reference container present).

## 2. Throughput and latency (ms), measured window only

| run | class | n ok | errors | per s | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|
| hyb-none [DIRTY] | r1_point | 26159 | - | 218.0 | 2.27 | 16.45 | 29.18 | 133.55 |
| hyb-none [DIRTY] | r1_keyset | 25934 | - | 216.1 | 6.06 | 23.91 | 40.06 | 188.59 |
| hyb-none [DIRTY] | r2_page | 87 | - | 0.7 | 112.32 | 144.14 | 181.79 | 181.79 |
| hyb-none [DIRTY] | t_order | 8392 | - | 69.9 | 13.64 | 66.74 | 104.67 | 242.18 |
| hyb-batched [DIRTY] | r1_point | 20153 | - | 167.4 | 3.45 | 19.38 | 69.99 | 469.64 |
| hyb-batched [DIRTY] | r1_keyset | 20145 | - | 167.3 | 7.04 | 27.17 | 75.77 | 457.89 |
| hyb-batched [DIRTY] | r2_page | 66 | - | 0.5 | 133.24 | 218.74 | 627.10 | 627.10 |
| hyb-batched [DIRTY] | t_order | 3363 | - | 27.9 | 17.30 | 408.31 | 783.99 | 1228.96 |
| hyb-single [DIRTY] | r1_point | 19997 | - | 160.4 | 4.09 | 19.97 | 36.45 | 3020.14 |
| hyb-single [DIRTY] | r1_keyset | 19959 | - | 160.1 | 7.82 | 27.67 | 50.17 | 3029.17 |
| hyb-single [DIRTY] | r2_page | 52 | - | 0.4 | 120.97 | 213.48 | 518.51 | 518.51 |
| hyb-single [DIRTY] | t_order | 3598 | - | 28.9 | 21.66 | 101.36 | 1981.25 | 3036.43 |
| off-none [INVALID] | r1_point | 21516 | - | 179.3 | 4.46 | 21.11 | 35.11 | 102.16 |
| off-none [INVALID] | r1_keyset | 21456 | - | 178.8 | 7.73 | 26.57 | 44.16 | 177.14 |
| off-none [INVALID] | r2_page | 12 | - | 0.1 | 17124.00 | 18856.25 | 18856.25 | 18856.25 |
| off-none [INVALID] | t_order | 6405 | - | 53.4 | 20.33 | 81.88 | 124.82 | 217.64 |
| off-batched [INVALID] | r1_point | 16717 | - | 139.3 | 5.93 | 23.59 | 95.31 | 572.68 |
| off-batched [INVALID] | r1_keyset | 16763 | - | 139.7 | 9.16 | 30.05 | 91.91 | 574.57 |
| off-batched [INVALID] | r2_page | 11 | - | 0.1 | 19296.02 | 20021.01 | 20021.01 | 20021.01 |
| off-batched [INVALID] | t_order | 2631 | - | 21.9 | 28.18 | 469.23 | 757.11 | 1598.43 |
| off-single [INVALID] | r1_point | 18745 | - | 149.4 | 6.19 | 20.73 | 34.41 | 2817.86 |
| off-single [INVALID] | r1_keyset | 18745 | - | 149.4 | 9.65 | 26.97 | 43.48 | 2819.59 |
| off-single [INVALID] | r2_page | 11 | - | 0.1 | 19353.36 | 20961.94 | 20961.94 | 20961.94 |
| off-single [INVALID] | t_order | 2571 | - | 20.5 | 32.26 | 115.26 | 2755.66 | 3366.90 |
| pg-none [INVALID] | r1_point | 46862 | - | 390.5 | 1.51 | 4.70 | 6.62 | 17.41 |
| pg-none [INVALID] | r1_keyset | 46362 | - | 386.3 | 3.31 | 7.54 | 10.05 | 24.36 |
| pg-none [INVALID] | r2_page | 25 | {'53100': 1} | 0.2 | 8296.19 | 10686.05 | 11223.78 | 11223.78 |
| pg-none [INVALID] | t_order | 14696 | - | 122.5 | 11.54 | 20.06 | 25.58 | 93.76 |
| pg-batched [INVALID] | r1_point | 46048 | - | 383.0 | 1.46 | 4.86 | 7.16 | 18.11 |
| pg-batched [INVALID] | r1_keyset | 45469 | - | 378.2 | 3.38 | 8.16 | 11.52 | 33.24 |
| pg-batched [INVALID] | r2_page | 20 | {'53100': 65} | 0.2 | 8526.93 | 10785.16 | 11980.03 | 11980.03 |
| pg-batched [INVALID] | t_order | 14017 | - | 116.6 | 12.15 | 22.16 | 28.87 | 188.76 |
| pg-single [INVALID] | r1_point | 46757 | - | 378.7 | 1.34 | 4.70 | 6.83 | 17.12 |
| pg-single [INVALID] | r1_keyset | 46249 | - | 374.6 | 3.19 | 7.86 | 11.24 | 28.55 |
| pg-single [INVALID] | r2_page | 24 | {'53100': 96} | 0.2 | 7809.44 | 9084.56 | 9789.61 | 9789.61 |
| pg-single [INVALID] | t_order | 9739 | - | 78.9 | 11.50 | 20.70 | 27.57 | 9403.48 |

## 3. Import transactions (latency of ONE transaction; the batch latency distribution)

| run | shape | import txns ok | errors | p50 | p95 | p99 | max | rows imported |
|---|---|---|---|---|---|---|---|---|
| hyb-batched | batched (5000 rows per transaction, 20 ms pause) | 112 | - | 644.61 | 1406.65 | 1566.74 | 2178.05 | 560000 |
| hyb-single | single (1% in ONE transaction, worst case) | 4 | - | 24417.71 | 48718.20 | 48718.20 | 48718.20 | 360000 |
| off-batched | batched (5000 rows per transaction, 20 ms pause) | 114 | - | 641.52 | 1283.37 | 1356.86 | 1718.09 | 570000 |
| off-single | single (1% in ONE transaction, worst case) | 2 | {'conn': 1} | 6821.50 | 48508.98 | 48508.98 | 48508.98 | 180000 |
| pg-batched | batched (5000 rows per transaction, 20 ms pause) | 142 | - | 568.16 | 811.34 | 955.95 | 970.44 | 710000 |
| pg-single | single (1% in ONE transaction, worst case) | 9 | - | 9027.82 | 11837.68 | 11837.68 | 11837.68 | 810000 |

## 4. Priority ratios (same engine and hybrid setting, import over no import; separate runs, one sample each)

p99 ratio = p99 with import / p99 of the `none` run; t_order ratio = throughput with import / without. Order of the runs is a confounder (see docs/32); a ratio of two single DIRTY runs says little.

| engine | import | p99 r1_point | p99 r1_keyset | p99 r2_page | t_order per s | t_order max ms |
|---|---|---|---|---|---|---|
| hyb | batched | 2.40 | 1.89 | 3.45 | 0.40 (27.9 / 69.9) | 1228.96 |
| hyb | single | 1.25 | 1.25 | 2.85 | 0.41 (28.9 / 69.9) | 3036.43 |
| off | batched | INVALID (none, batched) | - | - | - | - |
| off | single | INVALID (none, single) | - | - | - | - |
| pg | batched | INVALID (none, batched) | - | - | - | - |
| pg | single | INVALID (none, single) | - | - | - | - |

## 5. Hybrid path: used, fallbacks and their reasons, ring, merges

| run | used | fallbacks | state_ready | lag ms | merges | ring states | ring bases | idle trims |
|---|---|---|---|---|---|---|---|---|
| hyb-none | 133 | 0 | True | 0 | 0 | 1 | 1 | 1 |
| hyb-batched | 110 | 0 | True | 0 | 0 | 1 | 1 | 1 |
| hyb-single | 97 | 0 | True | 0 | 0 | 1 | 1 | 13 |

Reasons the path was not used (the server logs each reason once; the count per reason is not kept, only the total above):

- hyb-none: none logged
- hyb-batched: none logged
- hyb-single: none logged

## 6. State cost, RSS before and after hybrid (prelude)

- load of N=10000000 sims: 1088 s (rc 0), data directory 3092 MB after the load
- pristine STASK snapshot (reused by every STASK configuration instead of a reload): checkpoint 7 s (rc 0), clean stop rc 0, copy 7 s, 3092 MB on disk
- pristine PostgreSQL 16 snapshot (docker volume, reused by pg-* and the equality reference): load 466 s (rc 0), clean stop rc 0, 7089 MB
- restart without -hybrid: ready after 111.5 s, RSS 11729980 KB (11455 MiB), peak RSS 12003964 KB (11723 MiB)
- restart with -hybrid: ready after 132.0 s, RSS 12462588 KB (12170 MiB), peak RSS 12464932 KB (12173 MiB)
- RSS difference hybrid - not hybrid: 732608 KB (715 MiB); it is a difference of RSS (the Go heap may hold freed memory), not an exact size of the structures
- state build time (log line of the follower): `2026-11-01T04:43:46.182Z INFO  segcolumnar: bootstrapped at lsn=80024 in 14.966739469s`
- hybrid status after the restart: `{"applied_lsn": 80024, "durable_lsn": 80024, "fallbacks": 0, "lag_ms": 0, "merges": 0, "ring_bases": 1, "ring_idle_trims": 0, "ring_states": 1, "state_ready": true, "used": 0}`

## 7. r2 page 1-3 versus postgres:16 (equality run)

not run (N above PG_MAX_N, not asked for, or the run failed): there is no equality evidence in this report.

## 7b. Error samples (up to 3 distinct full messages per class)

| run | class | code | count | first s | message |
|---|---|---|---|---|---|
| hyb-none | r_join | 53200 | 2 | 2.5 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |
| hyb-batched | r_join | 53200 | 2 | 2.8 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |
| hyb-single | r_join | 53200 | 2 | 2.9 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |
| off-single | import | conn | 1 | 60.1 | timeout: context deadline exceeded |
| pg-none | r2_page | 53100 | 1 | 48.7 | ERROR: could not resize shared memory segment "/PostgreSQL.2587543428" to 6311936 bytes: No space left on device (SQLSTATE 53100) |
| pg-none | r_join | 53100 | 1 | 25.8 | ERROR: could not resize shared memory segment "/PostgreSQL.3141562230" to 8388608 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r2_page | 53100 | 1 | 47.9 | ERROR: could not resize shared memory segment "/PostgreSQL.1278781908" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r2_page | 53100 | 1 | 48.1 | ERROR: could not resize shared memory segment "/PostgreSQL.1012338014" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r2_page | 53100 | 1 | 48.2 | ERROR: could not resize shared memory segment "/PostgreSQL.4247386054" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r_join | 53100 | 1 | 48.1 | ERROR: could not resize shared memory segment "/PostgreSQL.1843147492" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r_join | 53100 | 1 | 48.7 | ERROR: could not resize shared memory segment "/PostgreSQL.4249969334" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-batched | r_join | 53100 | 1 | 50.1 | ERROR: could not resize shared memory segment "/PostgreSQL.3743674268" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r2_page | 53100 | 1 | 37.3 | ERROR: could not resize shared memory segment "/PostgreSQL.570257630" to 12615680 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r2_page | 53100 | 1 | 44.3 | ERROR: could not resize shared memory segment "/PostgreSQL.3860281854" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r2_page | 53100 | 1 | 44.4 | ERROR: could not resize shared memory segment "/PostgreSQL.1968102716" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r_join | 53100 | 1 | 45.0 | ERROR: could not resize shared memory segment "/PostgreSQL.1492079826" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r_join | 53100 | 1 | 46.1 | ERROR: could not resize shared memory segment "/PostgreSQL.378848986" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| pg-single | r_join | 53100 | 1 | 84.9 | ERROR: could not resize shared memory segment "/PostgreSQL.1529516226" to 4194304 bytes: No space left on device (SQLSTATE 53100) |

## 7c. Acceptance table (one row per rule; thresholds are the ones given by the project, none invented for R1 and T)

R2 p99 <= 1000 ms is the current reading of SLO-R2 and still to be confirmed by the user. The 1.25 x PostgreSQL 16 rule is evaluated only when the pg run of the same import shape exists and is not INVALID. A FAIL on a DIRTY run is a candidate, not a verdict.

**hyb-none [DIRTY]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | n/a (no import) |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | PASS (0) |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | 0.0000 (0 / 133); 0 distinct reason line(s) in hybrid_log.txt (no threshold set: record only) |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | PASS (181.8 ms) |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | no reference |
| p99 with import / p99 without import per class (same engine, record only) | n/a |

**hyb-batched [DIRTY]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | PASS (112 batches, 560000 rows, min 1 batch and 100000 rows; import errors 0) |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | PASS (0) |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | 0.0000 (0 / 110); 0 distinct reason line(s) in hybrid_log.txt (no threshold set: record only) |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | PASS (627.1 ms) |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | no reference |
| p99 with import / p99 without import per class (same engine, record only) | r1_point 2.40, r1_keyset 1.89, r2_page 3.45, t_order 7.49 |

**hyb-single [DIRTY]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | PASS (4 batches, 360000 rows, min 1 batch and 100000 rows; import errors 0) |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | PASS (0) |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | 0.0000 (0 / 97); 0 distinct reason line(s) in hybrid_log.txt (no threshold set: record only) |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | PASS (518.5 ms) |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | no reference |
| p99 with import / p99 without import per class (same engine, record only) | r1_point 1.25, r1_keyset 1.25, r2_page 2.85, t_order 18.93 |

**off-none [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |

**off-batched [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |

**off-single [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |

**pg-none [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |

**pg-batched [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |

**pg-single [INVALID]**

| rule | result |
|---|---|
| import ran (batches and rows >= minimum) | INVALID run |
| zero errors on r1_point, r1_keyset, r2_page, t_order, t_pay, t_cancel (any 53200 = FAIL) | INVALID run |
| hybrid fallback ratio fallbacks/(used+fallbacks) and reasons | INVALID run |
| R2 p99 <= 1000 ms (SLO-R2 interpretation, to be confirmed by the user) | INVALID run |
| p99 of r1_point, r1_keyset, r2_page, t_order <= 1.25 x the PostgreSQL 16 run of the same import shape | INVALID run |
| p99 with import / p99 without import per class (same engine, record only) | INVALID run |


## 7d. p99 during merges versus outside (staskd.log "tail merged into a new base"; seconds of the measured window that overlap a merge)

A merge interval is [log time - build time, log time] (the line is written when the merge ends). The per-second raw latencies (`series_raw.json.gz`) of the seconds that overlap an interval are pooled; p99 is of the pooled latencies. Merge counts come from `hybrid_log.txt` of the run.

| run | class | merges | seconds in merge | n in merge | p99 in merge ms | seconds outside | n outside | p99 outside ms |
|---|---|---|---|---|---|---|---|---|
| (no run has merge lines in hybrid_log.txt) | | | | | | | | |

## 8. What this report does not show

- Each cell is ONE run; no repetition, so no variance is known. Phase/order effects (a cold first run, page cache left by the previous run) are not separated. A run labelled DIRTY/INVALID must be repeated before it is quoted.
- The fallbacks reasons come from the first log line per reason; the data directory size is `du` of the STASK directory (or the PostgreSQL 16 data directory) at the end of the run; RSS is the staskd process at the end of the run.

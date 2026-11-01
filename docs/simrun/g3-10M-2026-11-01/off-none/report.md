# stask-m2 report: stask

- run: 2026-11-01T05:21:09Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 1.41 / max 8.54 / end 8.54, disk free 9.2 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 12 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 3 | 0 |  |
| no_orphan_order_items | 3 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 3 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 2 | 0 |  |
| r2_pages_prefix_consistent_frozen | 2 | 0 |  |
| reserved_stock_equals_open_items | 3 | 0 |  |
| stock_invariant_q13 | 3 | 0 |  |
| tear_no_order_without_items | 2 | 0 |  |
| tear_no_orphan_items | 2 | 0 |  |
| tear_order_total_equals_items | 2 | 0 |  |
| tear_q13_at_one_snapshot | 2 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 2 | 0 |  |
| tear_reserved_equals_open_items | 2 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 21456 | map[] | 178.8 | 7.73 | 26.57 | 44.16 | 177.14 |
| r1_point | 21516 | map[] | 179.3 | 4.46 | 21.11 | 35.11 | 102.16 |
| r2_page | 12 | map[] | 0.1 | 17124.00 | 18856.25 | 18856.25 | 18856.25 |
| r_join | 1 | map[] | 0.0 | 4005.43 | 4005.43 | 4005.43 | 4005.43 |
| t_cancel | 3205 | map[] | 26.7 | 18.98 | 76.56 | 110.35 | 193.20 |
| t_order | 6405 | map[] | 53.4 | 20.33 | 81.88 | 124.82 | 217.64 |
| t_pay | 3196 | map[] | 26.6 | 18.65 | 80.45 | 119.68 | 236.49 |
| tear_tx | 2 | map[] | 0.0 | 63644.16 | 65893.47 | 65893.47 | 65893.47 |

## Counters

- oracle_passes = 2
- t_attempt_err_40P01 = 1
- t_noop_cancel = 1321
- t_noop_order = 3342
- t_noop_pay = 1346
- t_retries = 1
- tear_snapshots = 2

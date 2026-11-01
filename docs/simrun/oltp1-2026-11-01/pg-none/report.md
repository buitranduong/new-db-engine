# stask-m2 report: pg16

- run: 2026-11-01T07:58:53Z, harness commit 8a73617, **DIRTY (load1 at start 2.27 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.27 / max 7.21 / end 7.21, disk free 2.2 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 29 | 0 |  |
| no_orphan_order_items | 29 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 29 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 29 | 0 |  |
| r2_pages_prefix_consistent_frozen | 29 | 0 |  |
| reserved_stock_equals_open_items | 29 | 0 |  |
| stock_invariant_q13 | 29 | 0 |  |
| tear_no_order_without_items | 37 | 0 |  |
| tear_no_orphan_items | 38 | 0 |  |
| tear_order_total_equals_items | 37 | 0 |  |
| tear_q13_at_one_snapshot | 38 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 37 | 0 |  |
| tear_reserved_equals_open_items | 37 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 32268 | map[] | 537.7 | 1.89 | 3.75 | 5.42 | 12.80 |
| r1_point | 32088 | map[] | 534.7 | 0.76 | 1.96 | 3.29 | 13.46 |
| r2_page | 147 | map[] | 2.4 | 789.41 | 838.12 | 897.24 | 903.78 |
| r_join | 16 | map[] | 0.3 | 207.45 | 397.58 | 397.58 | 397.58 |
| t_cancel | 5155 | map[] | 85.9 | 6.41 | 11.29 | 15.15 | 73.30 |
| t_order | 10393 | map[] | 173.2 | 7.16 | 12.95 | 17.23 | 76.74 |
| t_pay | 5239 | map[] | 87.3 | 6.47 | 11.44 | 15.25 | 68.00 |
| tear_tx | 30 | map[] | 0.5 | 1876.44 | 2012.47 | 2054.84 | 2054.84 |

## Counters

- oracle_passes = 28
- t_noop_cancel = 2131
- t_noop_order = 5578
- t_noop_pay = 2107
- t_retries = 0
- tear_snapshots = 37

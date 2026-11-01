# stask-m2 report: stask

- run: 2026-11-01T06:59:52Z, harness commit 8a73617, **DIRTY (load1 at start 1.15 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.15 / max 7.87 / end 7.87, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 10 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 10 | 0 |  |
| tear_no_order_without_items | 16 | 0 |  |
| tear_no_orphan_items | 16 | 0 |  |
| tear_order_total_equals_items | 15 | 0 |  |
| tear_q13_at_one_snapshot | 16 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 15 | 0 |  |
| tear_reserved_equals_open_items | 15 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 16434 | map[] | 273.9 | 5.32 | 14.78 | 23.92 | 75.09 |
| r1_point | 16309 | map[] | 271.8 | 1.53 | 9.63 | 16.84 | 54.64 |
| r2_page | 84 | map[] | 1.4 | 1347.58 | 1487.39 | 1522.28 | 1522.28 |
| r_join | 10 | map[] | 0.2 | 558.23 | 2256.37 | 2256.37 | 2256.37 |
| t_cancel | 2646 | map[] | 44.1 | 11.33 | 39.17 | 58.75 | 112.50 |
| t_order | 5342 | map[] | 89.0 | 12.54 | 42.15 | 66.68 | 140.06 |
| t_pay | 2692 | map[] | 44.9 | 11.46 | 39.16 | 60.76 | 115.24 |
| tear_tx | 12 | map[] | 0.2 | 4683.29 | 5137.23 | 5137.23 | 5137.23 |

## Counters

- oracle_passes = 10
- t_noop_cancel = 1156
- t_noop_order = 2875
- t_noop_pay = 1075
- t_retries = 0
- tear_snapshots = 15

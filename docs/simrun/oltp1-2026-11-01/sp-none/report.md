# stask-m2 report: stask

- run: 2026-11-01T08:12:03Z, harness commit 8a73617, **DIRTY (load1 at start 1.93 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.93 / max 6.97 / end 6.97, disk free 3.3 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 10 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 10 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 10 | 0 |  |
| r2_pages_prefix_consistent_frozen | 10 | 0 |  |
| reserved_stock_equals_open_items | 10 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 16 | 0 |  |
| tear_no_orphan_items | 16 | 0 |  |
| tear_order_total_equals_items | 16 | 0 |  |
| tear_q13_at_one_snapshot | 16 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 15 | 0 |  |
| tear_reserved_equals_open_items | 16 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 17014 | map[] | 283.5 | 5.18 | 14.27 | 22.97 | 62.36 |
| r1_point | 16870 | map[] | 281.1 | 1.33 | 9.09 | 15.51 | 70.38 |
| r2_page | 87 | map[] | 1.4 | 1297.53 | 1397.21 | 1410.39 | 1410.39 |
| r_join | 11 | map[] | 0.2 | 539.40 | 2313.98 | 2313.98 | 2313.98 |
| t_cancel | 3053 | map[] | 50.9 | 9.62 | 34.31 | 55.83 | 115.14 |
| t_order | 6115 | map[] | 101.9 | 10.71 | 35.96 | 56.02 | 110.86 |
| t_pay | 3063 | map[] | 51.0 | 9.42 | 33.04 | 52.18 | 116.10 |
| tear_tx | 12 | map[] | 0.2 | 4582.32 | 4805.18 | 4805.18 | 4805.18 |

## Counters

- oracle_passes = 10
- t_noop_cancel = 1209
- t_noop_order = 3138
- t_noop_pay = 1190
- t_retries = 0
- tear_snapshots = 15

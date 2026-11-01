# stask-m2 report: stask

- run: 2026-11-01T07:52:58Z, harness commit 8a73617, **DIRTY (load1 at start 1.97 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.97 / max 7.54 / end 7.54, disk free 3.4 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 19 | 0 |  |
| no_orphan_order_items | 19 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 19 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 19 | 0 |  |
| r2_pages_prefix_consistent_frozen | 19 | 0 |  |
| reserved_stock_equals_open_items | 19 | 0 |  |
| stock_invariant_q13 | 19 | 0 |  |
| tear_no_order_without_items | 36 | 0 |  |
| tear_no_orphan_items | 36 | 0 |  |
| tear_order_total_equals_items | 36 | 0 |  |
| tear_q13_at_one_snapshot | 36 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 36 | 0 |  |
| tear_reserved_equals_open_items | 36 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 19021 | map[] | 317.0 | 4.57 | 12.39 | 21.99 | 63.91 |
| r1_point | 18931 | map[] | 315.5 | 0.86 | 7.07 | 12.97 | 57.33 |
| r2_page | 549 | map[] | 9.1 | 13.31 | 21.32 | 27.66 | 56.30 |
| r_join | 61 | map[] | 1.0 | 633.87 | 7101.65 | 7213.22 | 7213.22 |
| t_cancel | 3598 | map[] | 60.0 | 8.00 | 28.27 | 46.42 | 111.79 |
| t_order | 7167 | map[] | 119.4 | 8.74 | 30.33 | 48.75 | 98.31 |
| t_pay | 3572 | map[] | 59.5 | 8.01 | 27.84 | 47.87 | 118.57 |
| tear_tx | 29 | map[] | 0.5 | 1974.53 | 2197.91 | 2240.67 | 2240.67 |

## Counters

- oracle_passes = 18
- t_noop_cancel = 1410
- t_noop_order = 3781
- t_noop_pay = 1431
- t_retries = 0
- tear_snapshots = 36

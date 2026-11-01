# stask-m2 report: stask

- run: 2026-11-01T08:46:40Z, harness commit 8775433, **DIRTY (load1 at start 1.89 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.89 / max 7.17 / end 7.17, disk free 3.1 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 11 | 0 |  |
| no_orphan_order_items | 11 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 11 | 0 |  |
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
| r1_keyset | 17749 | map[] | 295.8 | 4.99 | 13.47 | 21.86 | 95.38 |
| r1_point | 17518 | map[] | 291.9 | 1.17 | 8.52 | 14.20 | 61.24 |
| r2_page | 86 | map[] | 1.4 | 1309.03 | 1406.09 | 1449.05 | 1449.05 |
| r_join | 10 | map[] | 0.2 | 569.79 | 2301.69 | 2301.69 | 2301.69 |
| t_cancel | 3209 | map[] | 53.5 | 9.05 | 30.81 | 50.62 | 130.40 |
| t_order | 6422 | map[] | 107.0 | 10.03 | 34.35 | 53.71 | 120.39 |
| t_pay | 3214 | map[] | 53.6 | 9.27 | 31.14 | 48.49 | 132.26 |
| tear_tx | 12 | map[] | 0.2 | 4603.51 | 4895.05 | 4895.05 | 4895.05 |

## Counters

- oracle_passes = 10
- t_noop_cancel = 1256
- t_noop_order = 3354
- t_noop_pay = 1257
- t_retries = 0
- tear_snapshots = 15

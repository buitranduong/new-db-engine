# stask-m2 report: h2-rank-never

- run: 2026-11-01T11:36:41Z, harness commit , **DIRTY (load1 at start 1.02 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 1.02 / max 5.15 / end 5.15, disk free 2.6 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 14 | 0 |  |
| no_orphan_order_items | 14 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 14 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 14 | 0 |  |
| r2_pages_prefix_consistent_frozen | 14 | 0 |  |
| reserved_stock_equals_open_items | 14 | 0 |  |
| stock_invariant_q13 | 14 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 15594 | map[] | 389.8 | 3.71 | 8.90 | 17.34 | 72.04 |
| r1_point | 15485 | map[] | 387.1 | 0.39 | 4.08 | 10.52 | 50.04 |
| r2_page | 553 | map[] | 13.8 | 1.98 | 5.04 | 7.50 | 19.25 |
| r_join | 55 | map[] | 1.4 | 521.78 | 6662.73 | 6700.90 | 6700.90 |
| t_cancel | 3433 | map[] | 85.8 | 4.96 | 17.25 | 35.24 | 63.40 |
| t_order | 6804 | map[] | 170.1 | 5.53 | 19.25 | 35.28 | 74.87 |
| t_pay | 3373 | map[] | 84.3 | 4.96 | 17.41 | 37.25 | 81.82 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1266
- t_noop_order = 3534
- t_noop_pay = 1242
- t_retries = 0

# stask-m2 report: h2-hybrid-never

- run: 2026-11-01T11:52:06Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.99 / max 5.94 / end 5.94, disk free 2.6 GB

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
| r1_keyset | 15368 | map[] | 384.1 | 3.86 | 8.69 | 15.22 | 60.69 |
| r1_point | 15268 | map[] | 381.6 | 0.42 | 4.15 | 9.05 | 37.43 |
| r2_page | 459 | map[] | 11.5 | 10.77 | 16.52 | 20.74 | 26.04 |
| r_join | 47 | map[] | 1.2 | 524.87 | 6803.32 | 6839.80 | 6839.80 |
| t_cancel | 3299 | map[] | 82.5 | 5.62 | 16.91 | 28.37 | 66.81 |
| t_order | 6554 | map[] | 163.8 | 6.00 | 19.14 | 34.94 | 93.32 |
| t_pay | 3256 | map[] | 81.4 | 5.61 | 17.58 | 29.61 | 60.73 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1261
- t_noop_order = 3437
- t_noop_pay = 1234
- t_retries = 0

# stask-m2 report: h1-off-never

- run: 2026-11-01T10:54:29Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.90 / max 5.90 / end 5.90, disk free 2.6 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 8 | 0 |  |
| no_orphan_order_items | 8 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 8 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 7 | 0 |  |
| r2_pages_prefix_consistent_frozen | 7 | 0 |  |
| reserved_stock_equals_open_items | 8 | 0 |  |
| stock_invariant_q13 | 8 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 14877 | map[] | 371.8 | 4.15 | 9.02 | 13.57 | 36.66 |
| r1_point | 14816 | map[] | 370.3 | 0.51 | 4.59 | 8.93 | 27.43 |
| r2_page | 61 | map[] | 1.5 | 1179.65 | 1307.04 | 1319.99 | 1319.99 |
| r_join | 7 | map[] | 0.2 | 548.41 | 6988.14 | 6988.14 | 6988.14 |
| t_cancel | 3060 | map[] | 76.5 | 6.34 | 18.28 | 32.03 | 86.97 |
| t_order | 6063 | map[] | 151.5 | 6.94 | 20.22 | 34.20 | 102.53 |
| t_pay | 3003 | map[] | 75.1 | 6.33 | 17.94 | 30.66 | 68.31 |

## Counters

- oracle_passes = 7
- t_noop_cancel = 1159
- t_noop_order = 3155
- t_noop_pay = 1130
- t_retries = 0

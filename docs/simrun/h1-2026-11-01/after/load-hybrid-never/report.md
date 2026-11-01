# stask-m2 report: h1-hybrid-never

- run: 2026-11-01T11:08:44Z, harness commit , **clean**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 0.99 / max 5.92 / end 5.92, disk free 2.5 GB

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
| r1_keyset | 15422 | map[] | 385.5 | 3.81 | 8.88 | 14.21 | 54.19 |
| r1_point | 15329 | map[] | 383.2 | 0.42 | 4.25 | 9.45 | 29.10 |
| r2_page | 444 | map[] | 11.1 | 11.01 | 16.76 | 24.12 | 26.16 |
| r_join | 44 | map[] | 1.1 | 571.55 | 6762.90 | 6818.50 | 6818.50 |
| t_cancel | 3275 | map[] | 81.9 | 5.51 | 17.66 | 32.36 | 61.52 |
| t_order | 6456 | map[] | 161.4 | 6.25 | 18.95 | 33.20 | 99.81 |
| t_pay | 3177 | map[] | 79.4 | 5.58 | 18.03 | 31.70 | 84.19 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1206
- t_noop_order = 3371
- t_noop_pay = 1224
- t_retries = 0

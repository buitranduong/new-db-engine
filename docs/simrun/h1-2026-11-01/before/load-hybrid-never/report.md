# stask-m2 report: h1-hybrid-never

- run: 2026-11-01T10:48:42Z, harness commit , **DIRTY (load1 at start 1.03 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 1s, hot set 200 at 30%; 40 s elapsed
- machine: 8 CPUs, load1 start 1.03 / max 5.13 / end 5.68, disk free 2.6 GB

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
| r1_keyset | 15344 | map[] | 383.6 | 3.80 | 8.87 | 14.65 | 52.51 |
| r1_point | 15247 | map[] | 381.1 | 0.41 | 4.59 | 9.92 | 64.49 |
| r2_page | 416 | map[] | 10.4 | 10.95 | 16.27 | 19.66 | 23.35 |
| r_join | 43 | map[] | 1.1 | 586.11 | 6898.36 | 7067.81 | 7067.81 |
| t_cancel | 3250 | map[] | 81.2 | 5.39 | 17.86 | 30.51 | 61.96 |
| t_order | 6498 | map[] | 162.4 | 5.92 | 19.70 | 36.55 | 91.51 |
| t_pay | 3236 | map[] | 80.9 | 5.52 | 18.62 | 31.19 | 88.98 |

## Counters

- oracle_passes = 13
- t_noop_cancel = 1251
- t_noop_order = 3388
- t_noop_pay = 1185
- t_retries = 0

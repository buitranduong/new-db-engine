# stask-m2 report: stask

- run: 2026-11-01T08:17:36Z, harness commit 8a73617, **DIRTY (load1 at start 2.12 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=0, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 2.12 / max 5.55 / end 5.75, disk free 3.3 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 16 | 0 |  |
| no_orphan_order_items | 16 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 16 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 15 | 0 |  |
| r2_pages_prefix_consistent_frozen | 15 | 0 |  |
| reserved_stock_equals_open_items | 16 | 0 |  |
| stock_invariant_q13 | 16 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 27856 | map[] | 464.2 | 3.12 | 6.17 | 9.81 | 36.65 |
| r1_point | 27796 | map[] | 463.2 | 0.28 | 2.10 | 5.19 | 21.18 |
| t_cancel | 6919 | map[] | 115.3 | 3.49 | 9.66 | 17.29 | 45.19 |
| t_order | 13869 | map[] | 231.1 | 3.88 | 10.97 | 21.36 | 70.44 |
| t_pay | 6940 | map[] | 115.7 | 3.48 | 9.77 | 18.09 | 43.25 |

## Counters

- oracle_passes = 15
- t_noop_cancel = 2485
- t_noop_order = 7565
- t_noop_pay = 2567
- t_retries = 0

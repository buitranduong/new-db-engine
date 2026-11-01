# stask-m2 report: stask

- run: 2026-11-01T07:09:50Z, harness commit 8a73617, **DIRTY (load1 at start 1.02 > 1.0)**
- scale: 1000000 sims (4300200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 60 s elapsed
- machine: 8 CPUs, load1 start 1.02 / max 7.11 / end 7.11, disk free 3.4 GB

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
| reserved_stock_equals_open_items | 11 | 0 |  |
| stock_invariant_q13 | 11 | 0 |  |
| tear_no_order_without_items | 16 | 0 |  |
| tear_no_orphan_items | 16 | 0 |  |
| tear_order_total_equals_items | 16 | 0 |  |
| tear_q13_at_one_snapshot | 16 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 16 | 0 |  |
| tear_reserved_equals_open_items | 16 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 1000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 17456 | map[] | 290.9 | 4.97 | 13.69 | 22.14 | 69.04 |
| r1_point | 17266 | map[] | 287.7 | 1.35 | 8.75 | 15.06 | 48.45 |
| r2_page | 88 | map[] | 1.5 | 1279.71 | 1371.41 | 1613.46 | 1613.46 |
| r_join | 11 | map[] | 0.2 | 559.03 | 2217.54 | 2217.54 | 2217.54 |
| t_cancel | 3140 | map[] | 52.3 | 9.92 | 33.15 | 51.09 | 91.75 |
| t_order | 6188 | map[] | 103.1 | 10.50 | 35.27 | 54.29 | 106.98 |
| t_pay | 3049 | map[] | 50.8 | 9.40 | 33.12 | 48.35 | 81.11 |
| tear_tx | 13 | map[] | 0.2 | 4527.19 | 4824.65 | 4824.65 | 4824.65 |

## Counters

- oracle_passes = 10
- t_noop_cancel = 1185
- t_noop_order = 3221
- t_noop_pay = 1176
- t_retries = 0
- tear_snapshots = 16

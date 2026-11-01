# stask-m2 report: stask

- run: 2026-11-01T04:58:29Z, harness commit 6afb539, **DIRTY (load1 at start 1.83 > 1.0)**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import never every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 1.83 / max 8.39 / end 8.33, disk free 8.5 GB

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 5 | 0 |  |
| no_orphan_order_items | 6 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 5 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 5 | 0 |  |
| r2_pages_prefix_consistent_frozen | 5 | 0 |  |
| reserved_stock_equals_open_items | 5 | 0 |  |
| stock_invariant_q13 | 6 | 0 |  |
| tear_no_order_without_items | 5 | 0 |  |
| tear_no_orphan_items | 5 | 0 |  |
| tear_order_total_equals_items | 5 | 0 |  |
| tear_q13_at_one_snapshot | 5 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 5 | 0 |  |
| tear_reserved_equals_open_items | 5 | 0 |  |

All oracles held.

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| r1_keyset | 25934 | map[] | 216.1 | 6.06 | 23.91 | 40.06 | 188.59 |
| r1_point | 26159 | map[] | 218.0 | 2.27 | 16.45 | 29.18 | 133.55 |
| r2_page | 87 | map[] | 0.7 | 112.32 | 144.14 | 181.79 | 181.79 |
| r_join | 9 | map[53200:2] | 0.1 | 8180.92 | 13037.97 | 13037.97 | 13037.97 |
| t_cancel | 4173 | map[] | 34.8 | 12.34 | 60.77 | 96.08 | 153.16 |
| t_order | 8392 | map[] | 69.9 | 13.64 | 66.74 | 104.67 | 242.18 |
| t_pay | 4220 | map[] | 35.2 | 12.76 | 63.88 | 103.35 | 211.59 |
| tear_tx | 5 | map[] | 0.0 | 30179.32 | 32024.07 | 32024.07 | 32024.07 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r_join | 53200 | 2 | 2.5 | ERROR: out of memory: the query holds more than its budget of 2005 MB (-query-mem-mb) (SQLSTATE 53200) |

## Counters

- oracle_passes = 5
- t_noop_cancel = 1664
- t_noop_order = 4232
- t_noop_pay = 1664
- t_retries = 0
- tear_snapshots = 5

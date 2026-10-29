# STASK — prototype "log là nguồn sự thật duy nhất"

Prototype nghiên cứu cho `STASK_db_platform_research.md` + `stask_addendum_a_workload_consistency_spec.md`.

Một tiến trình (`staskd`) có **một log có thẩm quyền** (vừa là WAL vừa là kho giá trị), chỉ mục chính MVCC, và ba biểu diễn dẫn xuất (cột / full-text / time-series) được nuôi một chiều từ chính log đó. Planner chọn đường thực thi theo **yêu cầu nhất quán của truy vấn** và trả về kế hoạch đã dùng (engine, LSN, độ trễ view).

| Tài liệu | Nội dung |
|---|---|
| `docs/01_problem_invariants.md` | Giai đoạn A: bài toán, giả định ngầm, bất biến, mô hình lỗi, giả thuyết |
| `docs/02_candidate_architectures.md` | Giai đoạn B–C: 4 kiến trúc ứng viên, phản biện, lựa chọn |
| `docs/03_transaction_recovery.md` | Thiết kế log, commit, MVCC, checkpoint, phục hồi, view, planner |
| `docs/04_results.md` | Giai đoạn D: kết quả đo, so sánh PostgreSQL, thất bại, giới hạn, khuyến nghị |

## Cấu trúc mã

```
internal/wal      log phân đoạn, frame CRC32C, group-commit I/O, quét, cắt đuôi rách (punch hole)
internal/core     engine: committer, MVCC/SI/Serializable, B-tree chỉ mục, checkpoint, phục hồi, follower
internal/proj     view dẫn xuất: columnar, search (BM25 + fuzzy + bỏ dấu TV), time-series (nén)
internal/planner  hợp đồng nhất quán strong/bounded/session/eventual, chọn view hay core
internal/server   HTTP/JSON API, giao dịch tương tác, admin (checkpoint, fault injection)
cmd/staskd        server
cmd/staskbench    harness WL-1..WL-5 (HTTP hoặc nhúng in-process cho WL-1)
cmd/staskchaos    WL-6: kill -9 / ENOSPC / EIO + kiểm chứng mọi giao dịch đã ACK
bench/pg          baseline PostgreSQL 16 (pgbench) cho WL-1, WL-2, WL-5
scripts/          bench.sh (chạy một workload trên server mới), sysmon.py
```

## Build & test

Yêu cầu: Linux (dùng `fdatasync`, `fallocate`), Go ≥ 1.23. Baseline cần Docker.

```sh
go build -o bin/ ./cmd/...
go test -race ./internal/...        # gồm: SI bank invariant, write-skew, torn tail, mô phỏng mất điện (40 lần)
```

## Chạy server

```sh
./bin/staskd -dir /data/stask -addr 127.0.0.1:17070
```

Ví dụ API:

```sh
# bảng có view cột
curl -s localhost:17070/v1/tables -d '{"name":"sales","key":"id","columnar":true,"columns":[
  {"name":"id","type":"int"},{"name":"region","type":"string"},{"name":"qty","type":"int"},{"name":"price","type":"float"}]}'
# ghi nguyên tử (trả về lsn = token read-your-writes)
curl -s localhost:17070/v1/write -d '{"ops":[{"op":"put","table":"sales","row":{"id":1,"region":"Hue","qty":3,"price":9.5}}]}'
# đọc điểm (strong), lịch sử, đọc tại thời điểm
curl -s 'localhost:17070/v1/get?table=sales&key=1'
curl -s 'localhost:17070/v1/history?table=sales&key=1'
curl -s 'localhost:17070/v1/get?table=sales&key=1&as_of_lsn=2'
# phân tích, cho phép trễ ≤ 1s (planner tự rơi về core nếu view trễ hơn)
curl -s localhost:17070/v1/query -d '{"kind":"aggregate","consistency":{"mode":"bounded","max_lag_ms":1000},
  "aggregate":{"table":"sales","group_by":["region"],"aggs":[{"fn":"sum","col":"qty"},{"fn":"avg","col":"price"}]}}'
# giao dịch tương tác (snapshot | serializable)
curl -s localhost:17070/v1/tx/begin -d '{"isolation":"serializable"}'   # → {"tx":N}
curl -s localhost:17070/v1/tx/N/get -d '{"table":"sales","key":1}'
curl -s localhost:17070/v1/tx/N/write -d '{"ops":[...]}'
curl -s -X POST localhost:17070/v1/tx/N/commit                          # 409 = xung đột, thử lại
curl -s localhost:17070/v1/status
```

Mặc định API không xác thực và chỉ nghe loopback. Để mở ra mạng cần token (`-http-token`, `-http-token-file`), Basic (`-http-basic`) hoặc mTLS (`-http-tls-client-ca`), nên kèm TLS (`-http-tls-cert/-key`); khi đó gửi `-H "Authorization: Bearer <token>"`. Healthcheck không cần token: `GET /v1/health`. Chi tiết: `docs/13_http_security.md`.

Mỗi câu trả lời truy vấn có `plan`: `engine` (core/columnar/search/timeseries), `mode`, `durable_lsn`, `applied_lsn`, `view_lag_ms`, `waited_ms`, `reason`.

## Tái lập benchmark

```sh
# STASK (mỗi lệnh khởi động server mới trên thư mục dữ liệu riêng, ghi JSON vào results/stask.jsonl)
scripts/bench.sh wl1 -- -wl 1 -n 1000000 -conc 32  -duration 30s
KEEP=1 scripts/bench.sh wl1 -- -wl 1 -n 1000000 -skip-load -conc 128 -duration 30s
KEEP=1 scripts/bench.sh wl1 -- -wl 1 -n 1000000 -skip-load -conc 512 -rate 20000 -duration 30s
./bin/staskbench -wl 1 -embedded /data/wl1-emb -n 1000000 -conc 128 -duration 30s   # không HTTP
scripts/bench.sh wl2 -- -wl 2 -n 5000000 -qconc 2 -bg-writes 2000 -duration 60s
scripts/bench.sh wl3 -- -wl 3 -n 300000 -qconc 4 -bg-writes 500 -duration 60s
scripts/bench.sh wl4 -- -wl 4 -conc 6 -qconc 2 -duration 60s
scripts/bench.sh wl5 -- -wl 5 -n 10000 -conc 64 -duration 30s
scripts/bench.sh wl5-hot -- -wl 5 -n 100 -conc 64 -duration 30s
# WL-6
./bin/staskchaos -mode mixed -cycles 30 -writers 16
# PostgreSQL baseline (Docker, cùng ổ đĩa)
bench/pg/run_baseline.sh
```

Biến môi trường: `DATA` (gốc thư mục dữ liệu, mặc định `/mnt/sda3/stask-data`), `PORT` (mặc định 17070), `KEEP=1` (giữ dữ liệu cũ).

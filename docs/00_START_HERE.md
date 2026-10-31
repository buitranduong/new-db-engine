# 00 — Đọc file này đầu tiên (bối cảnh, mục đích, cấu trúc)

File này để **bất kỳ phiên làm việc mới nào** (người hay agent, sau khi hết giới hạn hay đổi agent) nắm đủ ngữ cảnh mà không cần người dùng nhắc lại. Trạng thái nằm trong **git và docs/**, không nằm trong cuộc trò chuyện.

Thứ tự đọc: `00_START_HERE` → `27_handoff` (quy tắc người dùng + trạng thái sống) → `STATUS` → `AGENT_PROTOCOL` → `PROCESS` (nhật ký quyết định) → `14_plan_target_actual` (bằng chứng chi tiết) → `15_target_workload` → `16_tiered_engine_vision`.

## 0. Mô tả ngắn (người dùng chốt, điều chỉnh 2026-10-31)

STASK là một engine viết mới bằng Go (không dùng mã PostgreSQL), lấy cơ chế của **ba cơ sở dữ liệu: PostgreSQL** (giao thức, SQL, giao dịch, chỉ mục, nhân bản; psql, pgjdbc, pgx, Liquibase dùng được nguyên vẹn), **ClickHouse** (view cột cho gộp, đếm, facet; chuỗi thời gian; lịch sử) và **Redis** (chỉ mục chính nằm trong RAM để tra cứu điểm nhanh). Một log bền duy nhất là nguồn sự thật, các cấu trúc còn lại dẫn xuất từ đó: một bản dữ liệu, một đường khôi phục, không phải đồng bộ giữa nhiều hệ thống.

**Elasticsearch không được coi là cơ sở dữ liệu và không đặt vào engine** (người dùng, 2026-10-31). Nghĩa là: Elasticsearch KHÔNG dùng làm chuẩn so sánh, tiêu chí hay hướng để phát triển mã tiếp. Mã đã có (view trigram, phần tìm kiếm trong `internal/proj`) GIỮ NGUYÊN, không xoá và không bị đổi vì lý do này. Bối cảnh ở §1 (hai cụm ES đang chạy) chỉ là hiện trạng.

Cách hiểu đúng các so sánh: STASK **cover chức năng** của các hệ đó cho bài toán này. "Đạt hiệu năng như" thì chưa chứng minh: số đo thật chỉ có sau đợt kiểm nghiệm cuối (`docs/24_final_measurement.md`, `PROCESS` D32/D34). Superset và Jmix không phải mục tiêu bắt buộc; Jmix chỉ là một ca thử driver.

## 1. Ai cần gì (bối cảnh thật)

Người đặt bài toán vận hành hệ thống bán SIM viễn thông:

- 3 hệ thống dùng chung dữ liệu: **Kho**, **CRM**, **website bán hàng**.
- **30–50 triệu số SIM**. Mỗi SIM có lịch sử các lần cập nhật, giữ **60 ngày gần nhất** (người dùng nói "hàng trăm lần cập nhật" mỗi SIM; con số này **chưa được xác nhận** và mâu thuẫn với "nhập 1–2 triệu dòng/ngày", xem docs/15 §0).
- Mỗi ngày nhập **hàng triệu bản ghi từ Excel** (giá, trạng thái, nhà mạng), đồng thời xử lý đơn hàng.
- Kiến trúc hiện tại: **1 PostgreSQL kho + 2 Elasticsearch** (website, tra cứu kho; đồng bộ bằng CDC) **+ 1 PostgreSQL riêng cho CRM + Superset** (thêm một lớp sao chép cho báo cáo).
- Nỗi đau: (1) PostgreSQL thường xuyên **lock bảng / request chờ nhau**; (2) **CDC sang ES trễ**, càng nhiều bản ghi càng trễ, và lỗi kết nối/dịch vụ gây **lệch dữ liệu**; (3) mỗi bản sao thêm (ES, Superset) là thêm chỗ trễ và lệch.

## 2. Mục đích của dự án (đã chốt với người dùng)

Không phải "một DB mới giải quyết mọi thứ" và không phải "một pipeline thực thi duy nhất". Mục đích là **một engine phân tầng, kiểm soát chặt, hiệu năng cao**, gom kiến trúc tốt nhất của các hệ hiện có, thay chuỗi PG + CDC + 2 ES + sao chép Superset bằng một hệ có các tầng với vai trò và độ trễ công bố rõ ràng (docs/16). Phải đạt mức **chạy được production**, nên kiểm thử nghiêm ngặt là một phần của sản phẩm.

Tên mã của engine: **STASK**. Ngôn ngữ: **Go 1.25** (Rust chỉ xét sau khi có số đo; docs/11).

## 3. STASK là gì (một đoạn)

Một tiến trình `staskd` có **một log có thẩm quyền** (vừa là WAL vừa là kho giá trị), chỉ mục chính MVCC trong RAM dựng lại được từ log, và các **view dẫn xuất** (cột, tìm kiếm full-text/trigram, time-series) được nuôi **một chiều** từ chính log đó, mỗi view có LSN và độ trễ công bố. Planner chọn đường đọc theo **mức nhất quán truy vấn đòi** (`strong`/`bounded`/`session`/`eventual`). Cổng vào: giao thức PostgreSQL (pgwire) và HTTP/JSON. Bất biến I1–I6: docs/01.

## 4. Cấu trúc repo

Repo: `github.com/buitranduong/new-db-engine` (nhánh `main` là nguồn sự thật). Mã chạy trên VM `ubuntu@192.168.0.220`, thư mục `~/stask`, các nhánh làm việc ở `~/wt/<tên>` (git worktree).

| Đường dẫn | Vai trò | Cỡ (dòng, không tính test) |
|---|---|---|
| `internal/wal` | Log phân đoạn, CRC32C, group commit, nén segment | ~1,7k |
| `internal/core` | Engine: committer, MVCC, checkpoint, phục hồi, GC, replica, PITR (`archive.go`), role/trigger/view hệ thống, hook `Quorum` | ~11k |
| `internal/sql` | Parser, executor, catalog (`pg_catalog`, `information_schema`), COPY, VIEW, TRIGGER, ALTER, role/quyền, admission | ~24k |
| `internal/pgwire` | Giao thức PostgreSQL (simple/extended), TLS, SCRAM, COPY | ~3,9k |
| `internal/proj` | View dẫn xuất: cột (kèm file nén `columnar.view`), search, time-series | ~6,5k |
| `internal/planner` | Hợp đồng nhất quán, chọn view hay core | ~0,6k |
| `internal/repl` | Nhân bản, base backup, giải mã logical (CDC) | ~1,9k |
| `internal/raft`, `internal/router` | Raft mô phỏng (chưa nối engine); bộ định tuyến đọc-ghi-của-mình | ~1,4k / 0,4k |
| `internal/server` | HTTP/JSON API, token/TLS, `/metrics`, quét CRC | ~1,9k |
| `internal/schema`, `conf`, `tune` | Schema/kiểu, cấu hình, tự chỉnh | ~1,3k |
| `cmd/staskd` | Server | |
| `cmd/claimcheck` | **Bằng chứng**: probe SQL cho từng tuyên bố trong docs (≥86 probe) | |
| `cmd/pgdiff` + `scripts/pgdiff/` | **So sánh trực tiếp với PostgreSQL 16** trên 859 câu SQL | |
| `cmd/simbench` | Benchmark kịch bản SIM (kho/CRM/web/nhập hàng/lịch sử) + `cmd/simfresh` (độ mới); đã chạy ở 200k-2M số, kết quả ở `docs/15 §6` và `docs/simrun/` | |
| `cmd/staskbench`, `staskmix`, `aggbench`, `staskbulk`, `staskchaos`, `staskstress`, … | Công cụ đo và chaos | |
| `scripts/` | `merge-gate.sh` (cổng trước khi gộp), `bench.sh`, `scale.sh`, `simbench.sh` | |
| `docs/` | Tài liệu (xem mục 6) | |

Tổng: khoảng 50k dòng mã, 11,7k dòng test (23%).

## 5. Quy ước vận hành bắt buộc

- **Build bằng `go build -o bin-main/ ./cmd/...`**, không dùng `bin/` (cũ). `go build ./...` không ghi binary; đã từng làm sai kết quả đo.
- **Không có bằng chứng thì không ghi "xong"** (docs/14). Bằng chứng là test, probe `claimcheck`, `pgdiff`, hoặc số đo tái lập được.
- **Mọi nhánh vào `main` qua `scripts/merge-gate.sh`** (gofmt, build, vet, test, `-race` tuỳ chọn, `claimcheck` không tụt).
- VM DÀNH RIÊNG cho dự án: 8 core, 31 GB RAM, đĩa 57 GB (hay đầy); đo hiệu năng chạy độc quyền trên chính VM này (D32), đĩa sẽ được cấp thêm ~50 GB ở bước đo cuối.

## 6. Bản đồ tài liệu

| File | Nội dung |
|---|---|
| `00_START_HERE` (file này) | Bối cảnh, mục đích, cấu trúc |
| `STATUS` | Đã đạt / chưa đạt, có số liệu |
| `AGENT_PROTOCOL` | Cách làm việc của agent; chống "phản biện chéo" |
| `PROCESS` | Nhật ký quá trình, quyết định, bài học |
| `01`–`04` | Bài toán, bất biến, kiến trúc ứng viên, thiết kế giao dịch/phục hồi, kết quả đo đầu tiên |
| `05` | SQL/pgwire: đã hỗ trợ gì (§ theo tính năng), TLS/SCRAM, role, metrics |
| `06` | Cluster: replica, định tuyến, Raft |
| `07`, `08` | Lưu trữ/dung lượng; bộ nhớ và GC |
| `09` | **Đặc tả định dạng đĩa/giao thức** và bộ nghiệm thu A1–A13 |
| `10` | Tương thích hệ sinh thái (CDC, Debezium) |
| `11` | Tiêu chí chuyển Go → Rust (ba cổng) |
| `12` | Kế hoạch đầy đủ P0–P9 (kích cỡ, phụ thuộc, tiêu chí xong) |
| `13` | Bảo mật HTTP |
| `14`, `14b` | **Kế hoạch – Mục tiêu – Thực tế** từng task + báo cáo so sánh PG |
| `15` | **Bài toán thật (SIM)**, ưu tiên theo nó, câu hỏi còn mở |
| `16` | Định hướng engine phân tầng, đường triển khai an toàn, danh sách kiểm production |

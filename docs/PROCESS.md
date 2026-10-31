# PROCESS — nhật ký quá trình, quyết định, bài học

Ghi theo thứ tự thời gian. Quyết định được **người điều phối** chốt ở mục 2; agent bám theo.

## 1. Quá trình đã thực hiện (2026-10-27 → 2026-10-29)

| Giai đoạn | Việc | Kết quả |
|---|---|---|
| Nền tảng (27-28/10) | Bài toán, bất biến, 4 kiến trúc ứng viên (docs/01-03); prototype `staskd`; benchmark WL-1..6; so PG16, Redis; chaos | Chọn kiến trúc A (log là nguồn sự thật); docs/04 |
| Lập kế hoạch (29/10) | Gom mọi giới hạn vào `docs/12` (P0-P9), cổng Go→Rust (`docs/11`) | Danh sách việc có cỡ, phụ thuộc, tiêu chí xong |
| Bảo mật & vận hành (P9) | TLS/mTLS cổng nhân bản, token/TLS HTTP, huỷ câu lệnh, timeout, ngân sách bộ nhớ, DROP COLUMN đánh dấu | Đã gộp; test qua |
| Gộp & sửa (29/10) | Gộp COPY, CREATE VIEW, catalog; **sửa data race PITR** (`-race` bắt được); sửa test hook chập chờn | Cây thống nhất, 12/12 gói test qua |
| Hạ tầng | `git init`, nhánh/worktree cho từng agent, remote GitHub, README, MIT, `merge-gate.sh`, CI (đang làm) | Push lên `main` |
| Song song đợt 1 | `admission`, `trigger`, `ops`, `cluster` gộp; `types`, `index` tiếp tục | `claimcheck` 27 → 31/46 |
| Song song đợt 2 | `pcol`, `pgdiff`, `harness` xong; `btree`, `spill`, `compat`, `e2e`, `raftwire`, `simbench`, `search`, `history`, `fixbatch`, `ci` chạy | Xem `STATUS §6` |
| Đối chiếu độc lập | Viết `claimcheck`, `pgdiff`; phát hiện README sai về cổng PG, binary cũ, đo hỏng | `docs/14` quy tắc "không bằng chứng thì không xong" |
| Làm rõ bài toán | Người dùng cung cấp bối cảnh SIM, PG+2ES+CRM+Superset, nỗi đau CDC trễ và khoá | `docs/15`, `docs/16`; ưu tiên được sắp lại |

## 2. Nhật ký quyết định (do người điều phối chốt)

| # | Quyết định | Lý do / bằng chứng | Trạng thái |
|---|---|---|---|
| D1 | Kiến trúc: log có thẩm quyền + view dẫn xuất một chiều + planner theo hợp đồng nhất quán | docs/01-02: giảm nguồn sự thật, trễ công bố, vận hành một binary | Giữ |
| D2 | Giữ Go; **hoãn Rust** tới sau khi có số đo hợp lệ | docs/11 §3: GC ≤ 1,1% CPU, pause ≤ 2,6 ms; nút thắt là thiết kế, không phải ngôn ngữ | Giữ |
| D3 | Định hướng: **engine phân tầng có kiểm soát**, không phải "một DB giải mọi thứ" hay "một pipeline thực thi" | Người dùng, docs/16 | Giữ |
| D4 | Ưu tiên theo bài toán SIM: index phụ, tìm kiếm từ SQL, lịch sử 60 ngày, nạp hàng loạt, `numeric` đúng; hạ P3.1, Raft, Rust, trigger PG | docs/15 §4 | Giữ, xem lại khi có số lịch sử thật |
| D5 | Trigger: **thân SQL**, không PL/pgSQL; `EXECUTE FUNCTION` chưa hỗ trợ | docs/10 §5; hệ quả: trigger sinh bởi Liquibase/Jmix sẽ không chạy, xem xét P7.2 nếu P4.2 cần | Giữ, rủi ro đã ghi |
| D6 | Cấu hình hệ thống (view SQL, trigger, role, trạng thái CDC) lưu trong **bảng hệ thống ẩn** đi qua WAL/checkpoint/replica | Một cơ chế, sống sót qua phục hồi và nhân bản | Giữ (mẫu cho tính năng sau) |
| D7 | DROP COLUMN chỉ đánh dấu; dòng cũ đọc theo lịch sử layout gắn LSN | docs/12 P1.2: không viết lại bảng | Giữ |
| D8 | Admission: đọc không bao giờ chờ slot; lane riêng cho heavy | docs/08; mô phỏng | Giữ; cần đo thật |
| D9 | Raft: gói riêng + hook `core.Quorum` mặc định tắt. **Hiện tự viết, nhưng đây là lựa chọn của agent, không phải yêu cầu của người dùng.** Người dùng chấp nhận học hỏi và dùng lại mã nguồn công khai; cần khảo sát thư viện có sẵn (etcd/raft, dragonboat, ...) và kiểm giấy phép | Một node không đổi hành vi | **Mở: chờ khảo sát docs/17** |
| D10 | Bằng chứng: không có probe/test/số đo thì không ghi xong (docs/14, docs/12 quy tắc 6) | Phát hiện báo cáo xong thiếu bằng chứng | Giữ |
| D11 | Build vào `bin-main/`, không dùng `bin/` | `go build ./...` không ghi binary; từng đo sai | Giữ |
| D12 | Mỗi task một nhánh + worktree; gộp bằng git qua `merge-gate.sh`; chỉ người điều phối gộp/push | Cây không có git trước đó; xung đột khi chép tay | Giữ |
| D13 | Cổng PG ngoài loopback không mật khẩu bị từ chối (`-pg-insecure` để bỏ qua) | README từng nói sai; lỗ hổng thật | Đã làm |
| D14 | Con đường triển khai: **chế độ bóng → thay tầng đọc (ES, Superset) → chuyển đường ghi → CRM** | docs/16 §4; giảm rủi ro, có đường lui | Chốt (người dùng giao quyền quyết) |
| D15 | Giấy phép MIT; repo `github.com/buitranduong/new-db-engine`; tài liệu tiếng Việt, README tiếng Anh | Người dùng | Đã làm |

| D16 | **Ưu tiên dùng lại, học từ mã nguồn công khai.** Không đặt "tự viết" làm mục tiêu. Thành phần có bản trưởng thành, giấy phép tương thích MIT (MIT/BSD/Apache-2.0) và phù hợp thì dùng hoặc tích hợp; được học thiết kế từ mã công khai (PostgreSQL, ClickHouse, Lucene, RocksDB, etcd...). Cấm chép mã GPL/AGPL/BSL/SSPL; ghi nguồn và giấy phép khi dùng | Người dùng làm rõ 2026-10-29: không yêu cầu tự viết; các agent chọn tự viết vì không chắc thành phần có sẵn đáp ứng | Giữ |

| D17 | **Phạm vi: chỉ bài toán kho SIM** (docs/15). KHÔNG làm phân mảnh/giao dịch xuyên shard, KHÔNG port Rust, KHÔNG nối index trên đĩa (`pindex`) vào engine trước khi có số lịch sử thật | 50M dòng vừa RAM (ước ~5-10 GB, chưa đo); Rust không có bằng chứng là nút thắt (docs/11 §3); `pindex` đo mới trên tmpfs, 1 luồng | Chốt |
| D18 | **Raft hiện có (`internal/raft`, `internal/cluster`) giữ nguyên, ĐÓNG BĂNG**: không thêm tính năng (đổi thành viên, đọc tuyến tính) và không thay bằng thư viện, cho tới khi `simbench` chứng minh giá trị một-nút | Đã qua chaos trên tiến trình thật (88.981 ghi xác nhận, 0 mất); thay thế tốn hơn lợi; HA không phải nút chặn hiện tại | Chốt |
| D19 | **Tìm kiếm mẫu số dùng view trigram trong SQL (nhánh `search`)**, không dựng Elasticsearch trong STASK; việc kế tiếp: đếm trực tiếp trên view khi view đã tới snapshot (khắc phục thua PG ở truy vấn trả nhiều dòng) | Đo 2M số: nhanh hơn quét 14-1500 lần, thắng PG+pg_trgm ở chuỗi dài/regex/số đẹp, thua 1,5-10 lần ở kết quả hàng chục nghìn dòng | Chốt |
| D20 | **Không viết `docs/17` khảo sát tổng quát.** Quyết "dùng lại hay tự viết" từng thành phần lúc chạm tới nó, bằng phép thử tích hợp nhỏ, theo D16 | Khảo sát chung tốn công, không cho bằng chứng; thành phần nào cần thì thử trực tiếp | Chốt |
| D21 | **Ưu tiên kế tiếp theo thứ tự:** (1) gộp mọi nhánh đã xong, (2) chạy `simbench` cỡ nhỏ và độ mới, (3) sửa diff `pgdiff` chặn ứng dụng: PK ghép/không PK (Liquibase), `integer`/`varchar(n)` đúng, ràng buộc kiểu, `$$`, CTE ghi, (4) lịch sử 60 ngày (`history`), (5) chế độ bóng PG→STASK | Mỗi mục chặn trực tiếp việc đưa STASK vào chạy thử trên dữ liệu thật | Chốt |
| D22 | Người điều phối tự quyết việc kỹ thuật/quy trình, không hỏi lại người dùng (AGENT_PROTOCOL §2b) | Người dùng phản hồi 2026-10-29 | Chốt |
| D23 | **`simbench`/`simfresh` là thước đo chính cho bài toán SIM** (docs/15 §6, số thô `docs/simrun/`). Kết quả đầu (200k-2M số, 8 vCPU dùng chung, mỗi ô một lần, chỉ tương đối): lên được cấu trúc tốt hơn PG ở tra số/tìm mẫu/nhập theo lô nhỏ, **thua ở 4 chỗ** (D24-D28) | Số đo | Chốt |
| D24 | **Sửa livelock của câu ghi dài khi có ghi đồng thời** (nhánh `occfix`): `INSERT..SELECT`/`UPDATE..FROM` 200k dòng bị OCC làm lại mãi (không xong trong 90-120 s khi có ~100 giao dịch/s ghi nền; khi rảnh 8,2 s, nhanh gấp 2 PG). Hướng: giải quyết xung đột **theo từng dòng** cho câu autocommit (ngữ nghĩa READ COMMITTED của PG: đọc lại phiên bản mới nhất và áp lại), không làm lại cả câu. Trước khi xong: hướng dẫn nhập theo lô 1.000-5.000 dòng (`batch_upsert`: STASK 19,6 s vs PG 24,1 s) | Đo `copy_merge`, `update_from` | Chốt |
| D25 | **Công bằng ở committer** (nhánh `commitfair`): đơn hàng chậm 4-10× (p50) khi đang nhập lúc tải đầy, PG gần như không đổi; nghi một committer tuần tự cộng chi phí nhiều index. Phải profile trước khi sửa, sửa bằng chia lô/ưu tiên giao dịch nhỏ/song song hoá `flushAdds`, mục tiêu đơn hàng p50 không quá 2× lúc nhập | Đo SR | Chốt |
| D26 | **Mặc định `stask.consistency` của SQL đổi thành `session`** (đọc-ghi-của-mình, chờ view tới LSN của chính phiên). Lý do: `bounded` mặc định chỉ thấy ngay 30-39% dòng vừa ghi; `strong` p99 ~300 ms; website/đơn hàng cần thấy dữ liệu mình vừa ghi | `simfresh` | Chốt |
| D27 | **`numeric` phải dùng được trên bảng columnar** (nhánh `colnumeric`): hiện bị từ chối, mà giá SIM là tiền; không có nó CRM GROUP BY chạy trên bảng dòng (chậm 1,7-2,3× PG) | `simbench` CRM | Chốt |
| D28 | **Khoảng trống SQL từ simbench** (nhánh `sqlgaps`): tham số trong danh sách cột của `INSERT..SELECT` (`08P01`) làm hỏng luồng đơn hàng; `NOT EXISTS` tương quan là tích lồng (>60 s so với 1,4 s bằng LEFT JOIN): cần giải tương quan thành semi/anti-join; `EXPLAIN` mọi loại câu và hiện index btree | `simbench` | Chốt |
| D29 | **P3.1 được định nghĩa lại: "chỉ mục có ngân sách bộ nhớ", không phải "thay RAM bằng đĩa"** (bổ sung D17; sửa 2 lần theo phản biện 2026-10-30; số liệu đã kiểm lại với docs/08 §5.4). **Bộ nhớ ước tính ở 50M số (ngoại suy tuyến tính, chưa đo ở 50M):** chỉ mục chính ≈ **2,1 GB** (docs/08 §5.4: `v4-dense` đã nối vào engine, entry 40 B không con trỏ, **41,6 B/khoá**, 832 MB ở 20M khoá; 79 B/khoá nếu độ đầy leaf chỉ 52% do thứ tự khoá, nên khoảng 2-4 GB tuỳ phân bố khoá); cột view cột ≈ 2,1 GB (0,83 GB ở 20M, tuỳ số cột); **view trigram ≈ 7,3 GB (145 MB/1M đo bởi `search`) = cấu trúc đơn lẻ lớn nhất, gấp ~3,5 lần chỉ mục chính**; btree phụ: **chưa đo**. (Các con số 123 B/khoá và 6,2 GB trước đây trong D29 là của giai đoạn cũ trước `v4-dense`: SAI, đã thay.) (a) **Việc đầu tiên là đo bộ nhớ từng cấu trúc** (nhánh `memmap`) bằng `inuse_space` của heap profile (không dùng hiệu RSS vì lẫn page cache và phần dư GC), ở **1M, 5M và 20M** (ba điểm để thấy đường có thẳng không; độ đầy B-tree đổi theo thứ tự nạp: 887 MB tuần tự, 2,2 GB ngẫu nhiên theo docs/18), và với **trigram đo trên số SIM có phân bố thật** (chung đầu số 090/091/098..., posting của trigram phổ biến rất dài), không dùng số ngẫu nhiên đều. Bộ nhớ của view trigram xếp ngang hàng với chỉ mục phụ: nén danh sách posting hoặc đưa xuống đĩa. Chỉ mục phụ lên `pindex` theo tuỳ chọn `storage='disk', cache_mb=N` (mặc định `ram`). (b) **Chỉ mục chính giữ cây RAM mặc định** (`pindex` chậm hơn ~1,4× ở đọc điểm kể cả khi đủ pool); `index_storage=disk` theo bảng là tuỳ chọn. (c) **Không tự động tràn**; chọn tường minh. Phải có `ALTER TABLE .. SET (index_storage='disk')` **chuyển ONLINE** (dựng lại từ cây RAM bằng `BulkLoad`), vì nếu chỉ chọn được lúc CREATE hoặc chạm trần chỉ biết từ chối ghi khoá mới thì đợt nhập Excel buổi sáng dừng = sự cố nghiệp vụ. Cảnh báo ở 80% kèm **dự báo** ("với tốc độ tăng hiện tại còn N ngày chạm trần"); từ chối ghi khoá mới chỉ là phương án cuối cùng. (d) **Không đưa cây RAM ra ngoài heap Go: GC đã nhẹ nhờ chỉ mục không con trỏ** (docs/08 §5.4: pha đánh dấu 40-44 ms, CPU GC 0,2-0,6% khi chạy tải 20M; trước đó 1.050-1.360 ms và ~6%). Lưu ý trung thực: số đó vẫn chưa loại trừ ảnh hưởng của pha đánh dấu chạy song song (write barrier, mark assist) lên đuôi độ trễ, và P0.5 (đọc p99 357 ms ở 100M) chưa có nguyên nhân. **Điều kiện mở lại (d):** chạy lại P0.5 bằng harness có đo chờ pool/CPU và thấy đuôi độ trễ trùng pha đánh dấu GC, HOẶC heap vượt ~10 GB. **Ba điều kiện BẮT BUỘC trước khi nối `pindex` vào engine:** (1) `BulkLoad` + `ALTER .. SET (index_storage)` online; (2) **latch lạc quan** (docs/18 §7), vì tìm kiếm website là đọc song song và `pindex` chỉ mở rộng 2,0-2,8× với 4 luồng do CAS ghim trang gốc (số đo nhiễu vì tải máy, RAM tree cùng lượt chỉ 2,2×: cần đo lại); (3) **đo trên đĩa thật**, trong khung không agent nào khác chạy và đủ chỗ trống, gồm kịch bản nhập Excel (cập nhật chỉ mục phụ ngẫu nhiên, nạp ngẫu nhiên chậm 3× khi dữ liệu vượt pool; nếu tệ thì gộp cập nhật chỉ mục phụ theo lô và sắp xếp trước khi ghi). Mọi số `pindex` hiện có đo trên tmpfs, load 22-40, nhiều agent chung máy | Phản biện 2026-10-30 và kiểm lại docs/08 §1/§5.4, docs/18, STATUS B5 | Chốt; làm sau khi các nhánh đang chạy gộp |

## 3. Bài học (đã trả giá)

1. **Binary cũ làm hỏng phép đo/probe**: luôn build mới, ghi commit vào kết quả (nhánh `harness` bắt buộc điều này).
2. **Báo cáo "xong" không đủ**: README viết theo docs đã sai về bảo mật; docs lệch mã. Giải pháp: docs/14, `claimcheck`, `pgdiff`.
3. **Đo hỏng im lặng** (`DROP TABLE` xoá nhầm bảng cột): harness phải tự kiểm tiền điều kiện (nhánh `harness`).
4. **Không có git thì không gộp an toàn nhiều nhánh**: dùng worktree + merge.
5. **Đĩa/CPU dùng chung làm số đo vô nghĩa và làm agent dừng**: cần máy riêng; kiểm `df` trước mọi việc lớn.
6. **Phản biện chéo giữa agent sinh nhiễu**: xem `AGENT_PROTOCOL §2`.
7. **Lỗi thao tác của người điều phối**: heredoc lồng làm file rỗng; `pgrep` tự khớp lệnh của mình rồi vẫn xoá; chờ tuần tự khi có thể song song; dấu nháy thừa làm cả lệnh ghi file thất bại. Biện pháp: kiểm điều kiện thật trước khi xoá; ghi file bằng một lệnh stdin riêng cho từng file; kiểm file sau khi ghi.
8. **Hệ thống chặn một hành động thì không vòng qua**: báo cáo và xin quyền.

## 4. Hàng việc kế tiếp (thứ tự do người điều phối chốt; đã cập nhật sau đợt gộp 2)

1. **`-race` trên bản gộp** (B11) rồi push; sau đó mỗi lần gộp chạy `merge-gate` với `RACE=1`.
2. **Khoá ẩn `__rowid` và PK ghép** để Liquibase khởi động được (chặn lớn nhất, pgdiff nhóm a).
3. **Chạy `simbench` cỡ 1M số** và viết phần đo độ mới (trễ view khi nhập, đọc-sau-ghi, `kill -9` giữa lúc nhập); sửa lỗ SQL phát sinh.
4. **`history`**: chính sách giữ lịch sử 60 ngày theo thời gian, nén, xoá theo ngày, truy vấn lịch sử từ SQL (agent đã giao, chưa có commit).
5. Sửa khác biệt `pgdiff` theo mức ảnh hưởng: OID `integer`/`varchar(n)`, ràng buộc kiểu, `$$`, CTE ghi, `SET (a,b)=(...)`, `to_char`, `AT TIME ZONE`; chạy lại `pgdiff` sau mỗi đợt; thêm bộ truy vấn Superset.
6. Đếm trực tiếp trên view trigram khi view đã tới snapshot (D19); entry index vào checkpoint; giảm chi phí ghi nhiều index.
7. **Chế độ bóng**: nhận dữ liệu PG→STASK (logical replication subscriber) + bộ so sánh kết quả truy vấn.
8. Máy đo riêng + benchmark chuẩn (YCSB, ClickBench); chuyển 38 script đo còn lại sang harness; lặp P0.5/P0.6/P0.8.
9. Chờ người dùng: số lịch sử thật, SLO độ mới, loại truy vấn Superset, ORM thực tế (docs/15 §5).

## D30 — Quyết định đã chấp nhận khi gộp integ3–5 (2026-10-30)
- Cursor: ngữ nghĩa khoá mềm (soft-lock) chấp nhận; đủ cho Liquibase/JDBC/SQLAlchemy.
- `selfAuthorized` cho DISCARD / RESET / SAVEPOINT: mọi role được chạy, không cần policy.
- Trường JSON `pk_name` của schema là thêm mới (omitempty), không phải đổi định dạng checkpoint.
- B16: dòng bảng tạm vẫn đi vào log. Lý do (txddl, bằng chứng): `core.Version` giữ `Loc/ValOff/ValLen` trỏ vào log, index không giữ giá trị; bỏ log cần kho giá trị thứ hai (đọc, GC, checkpoint, replica, drop). Không làm trong giai đoạn này; giảm nhẹ: bảng tạm ẩn khỏi CDC/publication/catalog phiên khác, bị xoá khi đóng phiên và quét khi khởi động.
- DDL trong giao dịch (nhánh txddl): CREATE TABLE / CREATE INDEX btree / ADD COLUMN áp ngay kèm undo; DROP TABLE / DROP INDEX / DROP COLUMN hoãn tới COMMIT; nhật ký 2 dòng trong `__stask_objs` (kind ddltx). Giới hạn ghi ở docs/05.

## D31 — Hai làn: hiệu năng tuần tự, phần còn lại song song (2026-10-30)
- Làn A (hiệu năng): memmap → simbench → 3 điều kiện D29 cho pindex → thiết kế GC lịch sử. Tuần tự, chỉ chạy trong cửa sổ yên tĩnh (không agent, không cổng đang chạy). Chủ: orchestrator.
- Làn B (tương thích, client, DDL, vận hành): agent song song, nhánh riêng, rebase lên main trước khi báo xong; gộp theo đợt nhỏ vào nhánh tích hợp, một cổng RACE + claimcheck mỗi đợt.
- Không chạy lệnh đụng cache/tài nguyên (go clean -cache, xoá dữ liệu) khi cổng đang chạy.
- Máy benchmark riêng vẫn là điều kiện để số đo hiệu năng có giá trị cuối cùng.
- D31 bổ sung (ba pha): (1) đo đường nền (ghi, index, log, GC lịch sử, bộ nhớ) ngay, vì quyết định kiến trúc; (2) Làn B phủ nốt tính năng có hạn chót, rồi đóng băng (tag) và gộp; (3) đo từng phần + chỉnh tối ưu SQL/Superset/trigram/cột trên bản đóng băng, mỗi chỉnh sửa có số trước/sau. Phát hiện kiến trúc ở pha 1 thì dừng tính năng mới vào vùng đó đến khi sửa xong.

## D32 — Đo hiệu năng dồn về cuối, phủ tính năng trước (2026-10-30, quyết định của người dùng)
- Thay D31 pha 1: không lặp vòng đo–sửa hiệu năng giữa chừng (mỗi vòng 3–10 phút, cả tiếng cho một vấn đề). Phủ tính năng Làn B trước (pgparity, txddl2, pgcmds), đóng băng, rồi đo và tối ưu từng phần.
- Ngoại lệ: chỉ sửa hiệu năng khi nó chặn đúng đắn hoặc là lỗi rõ ràng. `lanefix` (làn nặng) được đóng gói, gộp sau cổng, và các số đo chưa chạy lại ghi vào danh sách đo cuối.
- Danh sách đo cuối: simbench trên bản đóng băng (lanes: facet/đếm bị chậm khi làn nặng bão hòa; `-heavy-workers` mặc định; phiên bản cuối của khoá dạng truy vấn), memmap (công cụ chưa có), 3 điều kiện D29 cho pindex, GC lịch sử. Mọi phép đo vẫn qua scripts/perf-run.sh.

## D33 — Sau đợt phủ mã (2026-10-31)
- D18 sửa: Raft đã nối vào engine; đường HA chốt là cụm Raft 3 nút (bằng chứng: replica bất đồng bộ mất ghi đã xác nhận khi kill -9; cụm Raft 46 813 ghi xác nhận, 0 mất). Còn thiếu: đổi thành viên cụm, PITR trong cụm, kiểm với pgjdbc thật.
- `staskd -heavy-workers` mặc định giữ TẮT (0) cho tới khi đo cuối (D32): agent aggfacet đổi sang tự động nhưng chưa đo; Phụ lục O ghi p99 ghi 43 -> 313 ms ở 20M khi bật làn nặng.
- Chấp nhận phụ thuộc excelize v2.11.0 (BSD-3) và việc nâng x/crypto, x/net, x/sync, x/text, go 1.25.0.
- Chấp nhận thay đổi định dạng cộng thêm (omitempty/có phiên bản): IndexDef.Storage, Column.Collation, IndexDef.Colls, meta trigger/hàm nạp chồng, loại danh mục `collation`, cờ First của histstore. Bản cũ đọc dữ liệu mới không hỏng nhưng không thấy tính năng mới.
- Mặc định `-collation` = C. Bật `en_US.utf8` khớp 100% PG16 glibc trên 5 tập thử; chọn theo `datcollate` của PG production (chờ người dùng).

## D34 — Tiêu chí sẵn sàng cho prod, và các quyết định sau đợt phủ mã thứ hai (2026-10-31)

Engine được coi là sẵn sàng cho prod khi đạt CẢ BẢY tiêu chí (mỗi tiêu chí kiểm tra được bằng một phép thử):
1. Đúng: pgdiff với PostgreSQL 16 không còn khác biệt ngoài thông điệp lỗi; các driver PostgreSQL thật (pgjdbc, pgx) chạy đúng. Ứng dụng Jmix CRM chỉ là một bộ dữ liệu và ca thử tuỳ chọn, không bắt buộc.
2. Không mất dữ liệu: kill -9 ngẫu nhiên khi đang ghi, mất nút Raft, mọi ghi đã xác nhận còn nguyên; lặp lại dài ngày.
3. Khôi phục được: restore và PITR diễn tập trên bản sao dữ liệu prod, đúng số dòng, trong thời gian chấp nhận được.
4. Đạt tốc độ: ở 30-50M dòng, p99 tra cứu, thông lượng nhập Excel hằng ngày, truy vấn đếm/tìm kiếm đạt chỉ tiêu docs/ (đợt đo cuối, D32).
5. Chạy ổn định: tải thật liên tục tối thiểu 7 ngày, không rò bộ nhớ, không tăng độ trễ, không crash.
6. Vận hành được: giám sát, cảnh báo, nâng cấp không mất dữ liệu, tài liệu vận hành, đường lùi về PostgreSQL đã diễn tập.
7. Bảo mật: TLS/SCRAM/mTLS, audit log có chuỗi băm, backup mã hoá, volume dữ liệu mã hoá (LUKS2 là nền); `accept/security.sh` xanh.

Đường tới prod: chế độ bóng (2-4 tuần, prod vẫn là PostgreSQL) -> chuyển phần đọc có công tắc quay lại -> chuyển hẳn, giữ PostgreSQL làm đường lùi.

Quyết định kèm theo:
- Gate chạy một lần một phiên (flock); agent không chạy gate đầy đủ, orchestrator chạy một gate cho mỗi lần tích hợp. Bước `-race` có `internal/cluster`, timeout 40 phút.
- Mã hoá cấp engine (log, segment, chỉ mục) KHÔNG làm cho tới khi đo xong (đổi định dạng đĩa và đường mmap); nền là LUKS2 + backup AES-256-GCM.
- Superset và Jmix không phải mục tiêu bắt buộc (người dùng, 2026-10-31); niềm tin vào engine đến từ kiểm chứng đa agent (Claude, Gemini, ChatGPT), không phải thời gian chạy. Không điều chỉnh hướng trước khi có kết quả cuối của TẤT CẢ tiêu chí.
- Java không phải rào cản: pgjdbc/Hibernate thật chạy trong Docker (eclipse-temurin:17) trên VM, như một bài thử driver.
- Ngân sách thời gian (người dùng, 2026-10-31): khoảng 100 giờ xây engine, khoảng 300 giờ kiểm nghiệm trước prod, có thêm agent khác (Gemini, ChatGPT) cùng kiểm chứng. Đợt đo cuối vì vậy là một chương trình kiểm nghiệm dài; mọi phép thử phải tái lập được bởi agent không viết ra mã.

## D35 — Đích về duy nhất và luật dừng (2026-10-31, theo lo ngại của người dùng: nhiều tiêu chí mâu thuẫn, vòng lặp tối ưu hiệu năng, "đẽo cày giữa đường")

D35 THAY THẾ cách đọc nhiều tiêu chí rời rạc: bảy tiêu chí D34, bốn tiêu chí A-D và mười phép đo của `docs/24`, các bộ nghiệm thu, gate. Chúng không còn là danh sách song song mà xếp vào ba cổng. Muốn thêm tiêu chí mới phải bỏ hoặc gộp một tiêu chí cũ; mọi thứ ngoài ba cổng là backlog và KHÔNG chặn đích.

**Ba cổng, làm theo thứ tự:**
1. **G1 Đúng.** Gate xanh (`merge-gate.sh`, CI xanh), `pgdiff` + `scripts/suite` + `scripts/suite-cdc` + bộ pgjdbc thật không còn DIFF chưa giải thích; mỗi DIFF còn lại nằm trong bảng "khoảng trống đã biết" kèm lý do. (Gồm D34-1.)
2. **G2 An toàn.** Không mất dữ liệu đã xác nhận (kill -9, mất một nút Raft), restore và PITR diễn tập đúng số dòng, `accept/security.sh` xanh, backup mã hoá chạy. (Gồm D34-2, 3, 7.)
3. **G3 Đủ nhanh.** Chạy `docs/24` MỘT lượt. (Gồm D34-4, 5, 6.)

Đạt cả ba cổng = engine "xong" để vào chế độ bóng; chế độ bóng và chuyển prod là bước vận hành, không phải tiêu chí của engine.

**Khi các mục tiêu mâu thuẫn, thứ tự ưu tiên cố định:** đúng và không mất dữ liệu > bảo mật > tương thích chức năng > hiệu năng. Hiệu năng không bao giờ được đổi lấy tính đúng.

**Luật dừng vòng hiệu năng (chống đẽo cày):**
- Không có chỉ tiêu tuyệt đối từ người dùng thì tiêu chí duy nhất của G3 là "không tệ hơn PostgreSQL 16 (và ClickHouse khi so view cột) trên cùng dữ liệu, truy vấn, VM". Hơn nữa là tốt, không phải điều kiện.
- Mỗi hạng mục đo chỉ có tối đa HAI vòng sửa: đo -> sửa -> đo lại -> (sửa lần hai -> đo lại). Sau vòng hai mà còn tệ hơn: dừng, ghi vào bảng giới hạn của engine kèm số đo và nguyên nhân, và đi tiếp. Không mở vòng ba trừ khi người dùng yêu cầu.
- Không tối ưu ngoài danh sách đo của `docs/24`. Không tối ưu theo suy đoán; chỉ sửa chỗ số đo chỉ ra.
- Ngân sách thời gian cho G1-G3 nằm trong ~300 giờ kiểm nghiệm; gợi ý chia G1 ~100 giờ, G2 ~100 giờ, G3 ~100 giờ (người dùng có thể đổi). Hết ngân sách của một cổng mà chưa đạt: báo người dùng quyết định, không tự kéo dài.
- Mỗi lần có kết quả cuối của một cổng mới cho phép điều chỉnh thiết kế (D34); trong lúc chưa có thì không đổi hướng.


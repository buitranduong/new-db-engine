# 27. Bàn giao: khi người dùng chỉ nói "tiếp tục"

Tệp này là nơi DUY NHẤT một agent mới (Claude, Gemini, ChatGPT, hay phiên sau) cần đọc thêm sau `CLAUDE.md` và `00_START_HERE` để làm tiếp mà người dùng không phải nhắc lại. Bộ nhớ riêng của bất kỳ agent nào KHÔNG phải nguồn sự thật: mọi điều người dùng đã dặn phải nằm ở đây hoặc trong `PROCESS.md`. Người điều phối cập nhật §2 và §3 ở mỗi lần tích hợp vào `main`.

## 1. Quy tắc người dùng đã dặn (đứng vững cho đến khi họ đổi)

Cách làm việc:
- Tiếng Việt cho trao đổi và docs; README tiếng Anh. Báo cáo ngắn, có lập trường rõ ràng, không nước đôi; bằng chứng thắng ý kiến.
- Không hỏi người dùng về quyết định kỹ thuật, quy trình hay dọn dẹp: tự quyết và ghi vào `PROCESS.md`. Chỉ hỏi khi cần thứ chỉ họ có (dữ liệu thật, cấp tài nguyên, đổi mục tiêu).
- Làm song song những việc độc lập. Mọi nhiệm vụ có một dòng plan-target-actual trong `docs/14` (kể cả của người điều phối).
- Khi gặp khó (tương thích, test chập chờn, đồng thời, phục hồi, HA, di chuyển): TRA CỨU cách PostgreSQL, CockroachDB, ClickHouse… đã giải, rồi mới làm; ghi tiền lệ vào dòng `docs/14`. Chỉ giải pháp tối ưu hiệu năng mới có thể không có tiền lệ, và khi đó đo, không đoán (`AGENT_PROTOCOL §8`).
- Không đoán mò hướng đi và không tự đưa ngưỡng hay đổi chiến lược trước khi có kết quả cuối của TẤT CẢ tiêu chí (D34). Người dùng nói "tiếp tục" nghĩa là làm tiếp hàng việc ở §3, không phải bắt đầu hướng mới.
- Phạm vi: engine = cơ chế của hai cơ sở dữ liệu PostgreSQL + ClickHouse. Redis và Elasticsearch chỉ để học mô hình chỉ mục trên RAM nhằm tăng tốc; cú pháp của chúng quá hẹp nên KHÔNG dùng làm chuẩn, tiêu chí hay hướng phát triển; tiêu chí cho phạm vi cú pháp SQL là: PostgreSQL có thông dụng không. NHƯNG khả năng truy xuất phức tạp (wildcard, trọng số, tỉ lệ ưu tiên, kết hợp kiểu ES + ClickHouse trong SQL) là YÊU CẦU của engine (docs/29 R2). MÔ HÌNH TRUY VẤN ĐÍCH: một câu SQL nhiều điều kiện + sắp xếp + phân trang (`LIMIT/OFFSET` hoặc theo khoá, kèm tổng số dòng) như ClickHouse. Mã đã có GIỮ NGUYÊN. Superset và Jmix KHÔNG phải mục tiêu bắt buộc (Jmix chỉ là một ca thử driver). Niềm tin vào engine đến từ kiểm chứng đa agent; không dựa vào "năm tháng chạy thật". Ngân sách: ~100 giờ xây, ~300 giờ kiểm nghiệm trước prod.
- Hiệu năng: đo MỘT lần ở cuối khi mã đã đóng (D32, `docs/24`); ở các chỗ khác chỉ cần chạy đúng, không lỗi. Không xin thêm tài nguyên giữa chừng. Riêng đĩa: người dùng sẽ cấp thêm ~50 GB khi tới bước đo cuối cùng.
- ĐÍCH VỀ DUY NHẤT: ba cổng G1 Đúng, G2 An toàn, G3 Đủ nhanh (PROCESS D35). Thứ tự ưu tiên khi mâu thuẫn: đúng/không mất dữ liệu > bảo mật > tương thích > hiệu năng. Mỗi hạng mục hiệu năng tối đa hai vòng sửa rồi ghi giới hạn và đi tiếp; không thêm tiêu chí mới mà không bỏ hoặc gộp một cái cũ.
- Lời mô tả lại hay làm rõ của người dùng KHÔNG phải đổi hướng; chỉ "đổi / bỏ / thu gọn" mới đổi phạm vi. Khi người dùng cho thêm thông tin, cập nhật ngữ cảnh tại `docs/29_requirements.md` bằng văn xuôi rõ ràng (không chép nhật ký từng câu), không viết lại nhiều tài liệu. Mỗi việc liệt kê các yêu cầu liên quan nó có thể ảnh hưởng (đừng chăm chăm một tiêu chí).
- Số đo chỉ được báo ĐÚNG NHƯ ĐÃ ĐO (quy mô, tải máy, có sạch hay không). KHÔNG ngoại suy tuyến tính số đo nhỏ thành kết luận cho quy mô lớn hay cho hệ khác, và không dùng một lần đo nhiễu để kết luận về ClickHouse hay PostgreSQL. ClickHouse là chuẩn đối chiếu đã đáp ứng website. Ví dụ người dùng đưa ra là để minh hoạ sự cần thiết của engine, không phải để chứng minh hay bác bỏ một hệ nào. Mục tiêu của STASK ở truy vấn R2 là tiến gần mức của ClickHouse.
- ĐÓNG BĂNG tính năng tương thích PostgreSQL mới (2026-10-31, sau đợt `compat-*`): không giao thêm việc cú pháp/kiểu/hàm trừ khi một yêu cầu cụ thể từ lược đồ, truy vấn hay ứng dụng thật đòi hỏi. Trọng tâm là thực thi lai dưới bộ điều phối truy vấn (`docs/00_START_HERE.md §0`, `docs/30_hybrid_execution.md`): bộ điều phối chọn đường đọc, cột dùng được cho SQL/join/lọc/gộp, delta giao dịch (thấy ngay phần vừa ghi mà không chờ view hay quét lõi), cách ly tài nguyên thật (nhập, gộp, phân tích không tranh CPU/IO với đơn hàng, tồn kho, R1).
- MỘT PHÉP ĐO CHỈ HỢP LỆ KHI TẢI NÓ ĐỊNH ĐO THẬT SỰ ĐÃ CHẠY (bài học 2026-11-01: lượt `hyb-batched` ở 10M thoát mã 0 và báo "All oracles held" nhưng nhập hoàn tất 0 lô; oracle so tập rỗng với tập rỗng nên luôn đạt). Báo cáo đo không được gắn nhãn CLEAN nếu một lớp tải được cấu hình có ít thành công hơn ngưỡng tối thiểu, hay `kill -9` thực tế không xảy ra; harness phải ra INVALID và mã thoát khác 0. Khi báo bảng cho người dùng, đọc cột số lần thành công của TỪNG lớp trước, nêu cỡ mẫu, và không trình bày số của lượt không-có-tải như số của lượt có tải.
- Kỷ luật thí nghiệm (2026-11-01): ba lớp (lõi đóng băng, kernel tốc độ độc lập, tích hợp), mỗi giả thuyết có thí nghiệm + ngưỡng thắng + thời hạn, mỗi chu kỳ một nút thắt, không mang độ phức tạp vào main nếu không thắng rõ. Bảng hiện tại: `docs/34_decision_table.md`.
- Khi đo: `-scrub-interval 0` (quét nền mặc định bật), `-hybrid` có và không, máy yên, qua `scripts/perf-run.sh` (chi tiết `docs/24 §1b`). Kiểm CRC khi đọc log luôn bật; lỗi đọc là `XX001` rõ ràng, không bao giờ dữ liệu sai.
- Điều kiện nghiệm thu "hybrid": một truy vấn lớn chỉ được tính là đã đi đường hybrid khi `EXPLAIN` nói rõ nó đọc bao nhiêu dòng/byte từ lõi, từ delta, từ cột và vì sao chọn vậy; nếu chỉ là "Seq Scan + sort" thì chưa tính.
- Tài liệu repo phải đủ để đổi agent; đừng lưu quy tắc chỉ ở bộ nhớ riêng.

An toàn:
- Repo này CÔNG KHAI. Không đưa vào repo: mật khẩu, khoá, `application*.properties` thật, địa chỉ máy chủ production, bản backup, dữ liệu khách hàng. Dữ liệu khách hàng thật chỉ nằm trên VM (`~/crm-data`, container `prod-copy-pg`). Máy chủ production (SSH, thư mục backup) do người dùng cung cấp khi cần; chỉ dùng đọc, không thay đổi gì ở đó.
- Nếu một thao tác bị chặn (hệ thống phân quyền), KHÔNG tìm cách lách; báo lại. Dừng tiến trình theo PID do mình bật. Quét rộng `/tmp` dùng chung đã bị chặn; xin người dùng chạy lệnh hẹp hơn.
- Không đổi định dạng đĩa âm thầm (`docs/09_spec.md`); thay đổi chỉ thêm (omitempty/có phiên bản) được ghi ở `PROCESS` D33.
- Tải về thứ gì thì kiểm checksum.

## 2. Môi trường

- VM DÀNH RIÊNG cho dự án (không dùng chung): `ubuntu@192.168.0.220`, 8 vCPU, 31 GB RAM, đĩa 57 GB (~12 GB trống ở 2026-10-31). Repo `~/stask`, worktree `~/wt/<tên>`. Docker có sẵn và có mạng (dùng cho Java: `eclipse-temurin:17`; PostgreSQL tham chiếu: `postgres:16`).
- Mạng của máy người dùng đổi (192.168.0.x / 1.x / 50.x); nếu ssh quá hạn, bảo họ sửa mạng hoặc đưa địa chỉ VM mới.
- Cổng test bị va với dải cổng tạm của Linux. Gốc rễ đã xác định: `sysctl net.ipv4.ip_local_reserved_ports=21000-21999,28000-28999` (chỉ lúc chạy, mất khi khởi động lại VM; người dùng chưa quyết định ghi vào `/etc/sysctl.d`). Bộ test dùng khoá `flock` ở `/tmp/stask-test-ports`.
- GitHub: `buitranduong/new-db-engine`, CI ở `.github/workflows/ci.yml`. SAU MỖI LẦN ĐẨY phải xem CI xanh: không cần đăng nhập, dùng `https://api.github.com/repos/buitranduong/new-db-engine/actions/runs` và `.../check-runs/<job id>/annotations`; CI in dòng `FAIL` thành chú thích. Cấm đẩy dồn dập (mỗi lần huỷ lần trước).
- Cache Go: không xoá khi đang build/gate (đã từng làm hỏng gate); chỉ xoá mục cũ hơn 12 giờ, hoặc `go clean -cache` lúc VM rảnh.

## 3. Cách làm việc của người điều phối (vòng lặp)

1. Mỗi agent một worktree + một nhánh (`git worktree add ~/wt/<tên> -b <tên> main`); chỉ điều phối được gộp và đẩy `main`.
2. Agent KHÔNG chạy `scripts/merge-gate.sh` đầy đủ (gate chạy một lần một phiên nhờ `flock`; chạy song song đã làm quá tải VM). Agent chạy build, vet, test gói mình đụng tới, commit, báo cáo.
3. Điều phối gộp nhiều nhánh vào MỘT nhánh tích hợp (`merge --no-commit`; xung đột `.md` giải bằng `scripts/merge-docs.py`; KHÔNG BAO GIỜ commit xung đột mã), chạy `RACE=1 bash scripts/merge-gate.sh` MỘT lần, nếu xanh thì fast-forward `main`, ghi mốc `docs/claimcheck_baseline.txt` nếu tăng, thêm dòng vào `docs/14` (Phụ lục U), đẩy, rồi xem CI.
4. Chờ việc chạy nền bằng Monitor hoặc đọc file kết thúc (`GATE_EXIT`), không chờ thông báo mơ hồ. Không viết vòng chờ `pgrep -f`.
5. Sau khi gộp: gỡ worktree/nhánh, chạy `scripts/reap.sh` (`AGENT_PROTOCOL §9`). Agent đã báo cáo thì đóng; việc mới = agent mới với mô tả đầy đủ.
6. Khi người dùng hỏi tiến độ: bảng % theo hạng mục (nói rõ đó là ước lượng), việc đang chạy, việc cần họ.

## 4. Trạng thái sống (cập nhật 2026-10-31, sau integ19)

- `main` = `3971e83` (gate `RACE=1` xanh: 603/604 claimcheck, 1909 s; CI GitHub xanh). Không còn nhánh chờ gộp.
- Phần mã coi như đóng theo D35, trừ các khoảng trống ở §5. Chưa đo hiệu năng.
- Bộ nghiệm thu đã có: `scripts/pgdiff` (đối chiếu postgres:16), `scripts/suite` (2196 câu, 1698 giống PG16 sau `suitefix`; backlog ở `docs/25`), `scripts/suite-cdc` (11/11 đạt; `docs/26`), bộ pgjdbc/Hibernate/Liquibase thật (`accept/clients/pgjdbc-real`, `scripts/crmdocker`), `accept/security.sh`.
- Đang chạy: agent `g1` (worktree `~/wt/g1`) chạy lại mọi bộ nghiệm thu trên `main` và viết `docs/28_g1_status.md`, phân loại khác biệt còn lại thành A (kết quả sai, phải sửa), B (chưa hỗ trợ, người dùng chọn ưu tiên), C (chỉ khác thông điệp).
- Hàng việc tiếp: (1) nhận báo cáo `g1`; sửa nhóm A; đưa nhóm B cho người dùng chọn; (2) đóng G1 khi nhóm A rỗng và B/C đã giải thích; (3) G2 (diễn tập restore/PITR, `kill -9` lặp, mất nút Raft, bảo mật); (4) G3 theo `docs/24` (cấp thêm ~50 GB đĩa trước bậc 50M).
- Bài học quy trình: đẩy dồn dập huỷ lần chạy CI trước; mỗi tích hợp đẩy MỘT lần rồi chờ CI. Ghi dòng phụ lục `docs/14` TRƯỚC `git add` khi gộp.

## 5. Khoảng trống mã đã biết (cố ý chưa làm trước khi có số đo)

Raft (learner, chuyển leader chủ động); khoá dòng cho INSERT/MERGE/COPY và khoá trong savepoint đã rollback; row-level security (`CREATE POLICY` hiện báo lỗi rõ); nén posting trigram (có thiết kế, `docs/23`); file chỉ mục đĩa chưa tự co; mã hoá cấp engine (quyết định: nền là LUKS2 + backup AES-256-GCM, `docs/23`); kiểu composite còn thiếu `ALTER TYPE ADD ATTRIBUTE`; một số DDL còn áp ngay không hoàn tác khi ROLLBACK (`docs/14` Phụ lục SG); lỗi sẵn có: INSERT vào bảng phân vùng khi khoá chính gồm mọi cột.

## 6. Đang chờ người dùng (không chặn việc đang chạy)

- `SELECT datname, datcollate, datctype FROM pg_database;` trên PostgreSQL production (chọn `-collation`; mặc định C).
- Số thật: số phiên bản mỗi SIM trong 60 ngày, p99 mong muốn, tải đỉnh (`docs/15 §5`, `docs/19 §8`); có số thì thay tiêu chí "không tệ hơn PostgreSQL 16" của `docs/24`.
- Viết lại lịch sử git để bỏ hai nhị phân `staskd`/`claimcheck` đã lỡ commit (cần force push) — chưa quyết.
- Ghi `ip_local_reserved_ports` vào `/etc/sysctl.d` — chưa quyết.
- Xoá hay giữ `~/crm-data` và container `prod-copy-pg`.
- Cấp thêm ~50 GB đĩa trước bậc đo 50M (người dùng đã hứa khi tới bước cuối).
- Có làm mã hoá cấp engine không (đề xuất của điều phối: không, dùng LUKS2).

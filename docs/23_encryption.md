# 23 — Mã hoá sao lưu, mã hoá khi lưu (at rest), RLS, nén posting list

Mã: `internal/seccrypt`, `cmd/staskctl` (`main.go`, `sec.go`), `internal/core/archive.go`, cờ `staskd -archive-key-file`.

## 1. Mã hoá sao lưu và đoạn lưu trữ PITR (đã làm)

### Tiền lệ
| Hệ thống | Điều lấy về |
|---|---|
| pgBackRest | Mã hoá kho sao lưu, khoá/passphrase **không bao giờ nằm trong kho**; thuật toán AES-256-CBC (không xác thực) |
| WAL-G | Khoá từ tệp hoặc biến môi trường (`WALG_LIBSODIUM_KEY`, `_PATH`), mỗi đối tượng được mã hoá riêng, cả WAL lẫn base backup |
| age | Luồng chia khối 64 KiB (STREAM), khoá riêng cho từng tệp suy ra từ khoá chính + nonce ngẫu nhiên, khối cuối có cờ nên cắt cụt bị phát hiện |
| Google Tink streaming AEAD | Cùng cấu trúc với AES-GCM |

STASK theo age/Tink với **AES-256-GCM** (thư viện chuẩn Go, có phần cứng): `docs` của định dạng nằm ở đầu `internal/seccrypt/seccrypt.go`. Khoá riêng cho từng tệp = HKDF-SHA256(khoá chính, salt ngẫu nhiên 16 byte); nonce = chỉ số khối + cờ khối cuối; AAD = header. Header có "key check" (HMAC) nên sai khoá báo ngay `wrong encryption key` trước khi giải mã dữ liệu, còn sửa bit trong dữ liệu báo `authentication failed`.

### Dùng
- `staskctl keygen -out FILE` (0600, từ chối ghi đè) hoặc in ra stdout. Khoá 32 byte: 64 chữ số hex, base64, hoặc tệp thô 32 byte.
- Khoá lấy từ `-key-file` hoặc biến `STASK_BACKUP_KEY`; **không bao giờ lưu trong bản sao lưu**. Manifest chỉ có `key_id` (HMAC của hằng số, 6 byte) để biết cần khoá nào, và `manifest_mac` (HMAC của chính manifest) để không sửa được danh sách tệp/LSN.
- `staskctl backup|verify|restore|backup-loop`: có khoá thì mã hoá từng tệp của bản sao lưu (kể cả cụm Raft); `verify` và `restore` giải mã vào thư mục tạm riêng (chính nó bị xoá sau), sai khoá hoặc thiếu khoá thì dừng ngay ở bước kiểm khoá, không khởi động gì, restore không để lại tệp.
- Đoạn lưu trữ PITR: `staskd -archive-dir D -archive-key-file K` mã hoá từng đoạn WAL đã đóng; `-restore-archive D -archive-key-file K` giải mã. Khi có khoá, đoạn **không mã hoá** trong kho bị từ chối (chặn đoạn giả chèn vào) trừ khi `-archive-allow-plain` (kho có từ trước khi bật mã hoá); đoạn mã hoá mà không có khoá cũng bị từ chối.

### Kiểm chứng
- `go test ./internal/seccrypt`: vòng đi-về mọi cỡ quanh ranh giới khối (cả khi đọc từng byte), sai khoá, không phải tệp mã hoá, 11 kiểu phá (lật bit đầu/giữa/cuối/tag/header, cắt giữa khối, cắt đúng ranh giới, bỏ khối cuối, nối rác, nối khối chép, đảo hai khối, chỉ có header), bản mã không lộ chuỗi gốc, hai lần mã hoá khác nhau, đọc khoá từ tệp/env.
- `go test ./cmd/staskctl -run EncryptedBackup`: nút thật; bản sao lưu thường chứa dữ liệu (tiền đề), bản mã hoá không chứa dữ liệu lẫn khoá, không còn thư mục nháp, verify đúng khoá OK 50 dòng, sai khoá/thiếu khoá bị từ chối với thông báo rõ, sửa tệp bị manifest bắt, sửa tệp + dựng lại hash manifest bị MAC bắt, restore sai/thiếu khoá không để lại gì, restore bằng khoá từ biến môi trường rồi khởi động đọc được dòng cuối.
- `go test ./internal/core -run ArchiveEncrypted`: kho PITR mã hoá, không lộ plaintext, khôi phục đúng với khoá, sai khoá/thiếu khoá/sửa bit/đoạn plaintext chèn vào đều lỗi.
- `accept/security.sh` mục "encrypted backup": qua mTLS + secret của cổng nhân bản.

### Không chứng minh / còn thiếu
- Trong lúc backup, bản sao thô nằm tạm trong `<đích>.plain` (0700, xoá ngay sau khi mã hoá). Muốn không bao giờ chạm đĩa: đặt `-out` trên volume mã hoá (§2) hoặc tmpfs.
- Khôi phục đưa dữ liệu thô vào thư mục dữ liệu (engine đọc mmap): thư mục dữ liệu phải ở volume mã hoá.
- Chưa có xoay khoá tại chỗ (mã hoá lại bản cũ): xoay = tạo khoá mới cho bản sao lưu mới, giữ khoá cũ tới khi bản cũ hết hạn (như pgBackRest). Thay thế bản cũ do manifest đã MAC không phát hiện (tấn công quay lui bản sao lưu): cần kho bất biến/đối tượng có phiên bản.
- Không đo hiệu năng (theo AGENT_PROTOCOL §6).

## 2. Mã hoá dữ liệu khi lưu (at rest): thiết kế đề xuất, chưa cài engine-level

### Tiền lệ
| Hệ thống | Cách làm |
|---|---|
| PostgreSQL | **Không có TDE gốc** trong bản cộng đồng; khuyến nghị mã hoá hệ thống tệp/volume (LUKS, dm-crypt, EBS). Fork thương mại (EDB, Percona pg_tde, Cybertec) làm ở tầng trang/WAL |
| CockroachDB | "Encryption at Rest" (Enterprise): **store key** do người vận hành đưa vào (tệp), sinh **data key** xoay vòng, mã hoá từng tệp SST/WAL bằng AES-CTR, danh sách khoá ghi trong tệp registry được store key mã hoá |
| MySQL InnoDB | Tablespace encryption: khoá chính (keyring) mã hoá khoá của từng tablespace, mã hoá khi ghi trang xuống đĩa |
| MongoDB WiredTiger | Khoá chính (KMIP/tệp) mã hoá khoá từng cơ sở dữ liệu, AES-256-CBC/GCM |

### Khuyến nghị
1. **Nền tảng bắt buộc: LUKS2 (dm-crypt, AES-XTS-256) cho volume chứa `-dir`, `-archive-dir`, `-spill-dir`, thư mục nháp của staskctl và `-audit-dir`.** Bao phủ toàn bộ: WAL, checkpoint, view, cột, chỉ mục đĩa, tệp tạm (spill), log; không đụng đường đọc/ghi/mmap của engine (đây là hot path mmap, mã hoá trong engine sẽ phá `mmap` đọc trực tiếp và cần chép sang bộ đệm). Khoá: passphrase/keyfile ở `/etc/crypttab` hoặc TPM2/Clevis+Tang; không để keyfile trên chính volume. Swap mã hoá (hoặc tắt), `vm.swappiness` thấp, core dump tắt (RAM chứa dữ liệu thô).
2. **Lớp tuỳ chọn engine-level (làm sau, chỉ nếu có yêu cầu pháp lý mà LUKS không đáp ứng, ví dụ khoá riêng theo bảng hoặc tách quyền quản trị OS và quản trị DB):** theo CockroachDB: store key từ tệp/KMS → data key theo segment WAL/checkpoint (AES-256-GCM hoặc CTR + MAC theo khối, nonce = (segment, offset)), registry khoá nhỏ được store key bọc. Không làm trong lượt này vì: (a) log là nguồn sự thật và được mmap, mã hoá theo khối phải giải mã ra bộ đệm mới đọc được, ảnh hưởng đường đọc và phục hồi; (b) đổi định dạng đĩa (CLAUDE.md quy tắc 7: dừng và báo cáo, không đổi lặng lẽ); (c) nén segment (`zseg`) và scrub/GC/replication/backup đều đọc segment nguyên bản. Cần quyết định của người điều phối (định dạng, vị trí khoá, quan hệ với nén) trước khi làm.
3. Thứ KHÔNG được LUKS bảo vệ: kẻ chiếm được tiến trình hoặc OS khi volume đang mở; ảnh chụp volume đã mở; sao lưu/đường truyền (đã có §1 cho sao lưu và đoạn PITR, TLS cho truyền). Truyền: PG wire TLS+SCRAM, HTTP TLS, cổng nhân bản mTLS+secret (docs/13, `accept/security.sh`).

## 3. Row-level security (`CREATE POLICY`)

Kiểm tra bằng `TestRowLevelSecurityNotSupported` (`internal/sql/rls_probe_test.go`): `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` báo lỗi cú pháp, `CREATE POLICY` báo `only CREATE TABLE is supported` (không có từ chối lặng lẽ: chính sách không bao giờ bị "chấp nhận mà không thực thi"). `pg_policy` trong catalog rỗng. **Chưa cài đặt.** Không nhỏ: cần lưu chính sách (đối tượng), áp điều kiện USING/WITH CHECK vào mọi đường đọc/ghi (SELECT/UPDATE/DELETE/INSERT/MERGE/COPY/view/chỉ mục/CDC/`ON CONFLICT`), bỏ qua cho superuser/owner, ảnh hưởng planner (không đẩy vị từ qua hàm rò rỉ), nhiều đường truy cập không đi qua `Session` (HTTP `/v1/get`, view cột). Thiết kế đề xuất khi làm: viết lại AST ở `Session.exec` (AND policy vào WHERE của từng `FromTable`, như `viewDML`), bật theo bảng, mặc định từ chối khi có RLS mà không có policy (như PostgreSQL), kèm test ma trận role × lệnh × đường truy cập. Tạm thời dùng GRANT theo bảng (docs/05 §18) và view.

## 4. Nén posting list của trigram (chưa làm, có thiết kế)

Hiện trạng (`internal/proj/substr.go`, `substrdisk.go`): ở chế độ RAM mỗi trigram giữ danh sách doc id; ở chế độ đĩa (docs/18 §13) posting là **khoá trong B-tree pindex**: khoá = (trigram, doc), nên một posting = một khoá, kích thước ~ số (trigram, doc) cặp.

Tiền lệ: Lucene (khối 128 doc id: delta + bit-packing "PForDelta/FOR", skip list; khối cuối vbyte), Roaring bitmap (chia theo 16 bit cao, vùng thưa là mảng, vùng dày là bitmap/run; dùng ở Elasticsearch/Lucene cho filter cache, ClickHouse, Pilosa), Elias-Fano và varint-delta (Google, Zoekt dùng delta+varint cho trigram posting).

Lý do không làm trong lượt này: (1) ở chế độ đĩa posting là *khoá* của pindex, nén theo khối nghĩa là đổi sang "một giá trị = một khối posting" kèm cập nhật tại chỗ (đọc-sửa-ghi khối khi thêm/xoá doc), tức đổi định dạng pindex và logic `bulkBuild`/`listOf`/xoá, không phải thay đổi cộng thêm an toàn; (2) mọi đường truy vấn dựa vào duyệt khoá theo `iter`; (3) CLAUDE.md quy tắc 7. Đề xuất: giai đoạn A, chỉ chế độ RAM: danh sách doc id đã sắp xếp lưu delta+uvarint theo khối 128 với skip (doc đầu khối), giải mã khi giao nhau (merge-join hai danh sách) — thay đổi cục bộ, không đổi đĩa, kiểm bằng so sánh kết quả với bản không nén trên corpus ngẫu nhiên; giai đoạn B: chế độ đĩa với khoá (trigram, doc đầu khối) và giá trị = khối nén, sau khi người điều phối duyệt định dạng. Mọi con số về mức giảm cần đo trong pha đo cuối (AGENT_PROTOCOL §6), chưa có số nào.

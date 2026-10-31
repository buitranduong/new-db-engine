# 22 — Audit log (nhật ký kiểm toán chống sửa)

Mã: `internal/audit` (ghi, xác minh, truy vấn), móc vào `internal/sql/auditstmt.go` (câu lệnh), `internal/pgwire/audit.go` (kết nối), `internal/server/auditwrap.go` (HTTP và thành viên Raft), `cmd/staskctl/sec.go` (`staskctl audit verify|query`). Dữ liệu là dữ liệu SIM gắn với khách hàng nên ai làm gì, từ đâu, thành công hay không phải truy lại được và không sửa lén được.

## 1. Tiền lệ

| Hệ thống | Điều lấy về |
|---|---|
| PostgreSQL `log_connections`, `log_disconnections` | Mỗi lần kết nối (thành công và thất bại) và ngắt kết nối là một dòng; lý do thất bại ghi kèm |
| pgaudit | Lớp sự kiện (`READ, WRITE, ROLE, DDL, MISC`), ghi theo **phiên** (mọi bảng) hoặc theo **đối tượng** (chỉ bảng được chọn), không ghi mật khẩu, tham số mặc định không ghi |
| Certificate Transparency / Trillian | Log chỉ-thêm, mỗi bản ghi móc vào bản trước bằng băm: sửa/xoá/chèn đều lộ |
| auditd | Tệp chỉ-thêm; `chattr +a` và gửi ra máy khác là việc của người vận hành |

## 2. Cờ staskd

| Cờ | Ý nghĩa |
|---|---|
| `-audit-dir DIR` | Bật. Tệp `audit-NNNNNNNN.log` (JSON một dòng một bản ghi, mode 0600) và `audit.head` |
| `-audit-log` | Lớp: `connection,role,ddl,read,write,denied,admin,all,none`. Mặc định `connection,role,ddl,denied,admin`. `read`/`write` = ghi theo phiên cho **mọi** bảng |
| `-audit-tables` | Ghi theo đối tượng: `t1,t2:read,t3:write,t4:read+write`. Hoạt động cả khi lớp `read`/`write` tắt |
| `-audit-key-file` | Khoá HMAC cho chuỗi (>= 16 byte, thô hoặc hex). Không có thì cảnh báo khi khởi động |
| `-audit-max-mb`, `-audit-keep` | Xoay tệp theo kích thước (mặc định 64 MiB); giữ N tệp (0 = giữ hết) |
| `-audit-statement` | Thêm văn bản câu lệnh vào bản ghi đọc/ghi (có thể chứa dữ liệu khách; mặc định tắt, như `pgaudit.log_parameter`) |

## 3. Bản ghi

`seq, ts, ev, user, role, addr, app, cmd, obj, detail, ok, alg, prev, hash`.

| `ev` | Khi nào | Lớp |
|---|---|---|
| `connect.ok`, `connect.fail`, `disconnect` | Sau xác thực PG wire; thất bại gồm sai mật khẩu, role NOLOGIN, không có dòng pg_hba, thiếu TLS khi `-pg-tls-require`; `detail` có lý do | connection |
| `role` | CREATE/ALTER/DROP ROLE, GRANT, REVOKE. **Mô tả sinh từ cây cú pháp** (`login=yes password=set`), không bao giờ từ văn bản nên mật khẩu không thể vào log | role |
| `ddl` | CREATE/ALTER/DROP mọi thứ khác, TRUNCATE; có văn bản, trừ khi văn bản nhắc tới `password` (ghi `<text withheld>`: một chuỗi nhiều lệnh có thể chứa `CREATE ROLE ... PASSWORD`) | ddl |
| `sql.denied` | SQLSTATE 42501 | denied |
| `sql.read`, `sql.write` | SELECT, INSERT, UPDATE, DELETE, MERGE, TRUNCATE, COPY (một bản ghi cho mỗi bảng) | read/write hoặc `-audit-tables` |
| `http.auth_fail` (401), `http.denied` (403), `http.admin` (POST `/v1/admin/*`) | Danh tính: `cert:<CN>` khi mTLS, `token:<4 byte băm>` (không bao giờ là token), `basic`, `anonymous` | admin |
| `cluster.member` | `POST /v1/admin/cluster/members` (thêm/bỏ voter), kết quả và nút | admin |
| `audit.start`, `audit.rotate` | Mỗi lần mở log / xoay tệp, ghi thuật toán băm | luôn |

Bản ghi connection, role, ddl, denied, admin được `fsync` trước khi lệnh trả về; read/write được `fsync` trong 1 giây.

## 4. Chống sửa

`hash = H(prev || bản_ghi_không_có_hash)`; `H = HMAC-SHA256(khoá)` nếu có khoá, nếu không là SHA-256. Chuỗi liền qua các tệp (bản ghi đầu tệp mới là `audit.rotate`). `audit.head` giữ `(seq, hash, mac)` sau mỗi lần `fsync`.

`staskctl audit verify -dir DIR [-key-file F]` phát hiện: sửa một trường, xoá, chèn, đảo thứ tự, thiếu tệp giữa chừng, cắt đuôi (đối chiếu `audit.head`), head giả (MAC), log có khoá mà không đưa khoá. `staskctl audit query -dir DIR [-ev -user -obj -cmd -failed -since -until -last -json]` lọc bản ghi.

Khởi động lại: đọc bản ghi cuối để nối chuỗi; dòng cuối dở dang do sập máy bị cắt bỏ và ghi vào `audit.start`.

**Giới hạn (nói thẳng):**
- Không có khoá, ai ghi được vào thư mục có thể dựng lại cả chuỗi từ đầu; có khoá thì không (khoá nên nằm ngoài máy chủ DB, hoặc ít nhất khác user). Đây là tiền lệ của mọi log băm tự chứa: chống sửa **lộ ra**, không chống **xoá cả thư mục**. Muốn chống xoá: `chattr +a` hoặc chuyển tệp đã xoay sang máy khác/WORM và lưu `audit.head` ở nơi khác.
- Cắt đuôi chỉ lộ nếu `audit.head` còn nguyên (nó có MAC nên không giả được, nhưng xoá được).
- Siêu người dùng khởi động (mật khẩu chung `-pg-password`) được ghi là user gửi lên trong startup message, không phải danh tính đã chứng minh.
- Không ghi: `SET ROLE`/`SET SESSION AUTHORIZATION`, câu lệnh bên trong hàm PL/pgSQL và `WITH ... INSERT` theo từng bảng (ghi một bản ghi `WITH` không có bảng), câu lệnh đọc qua extended protocol bị từ chối ở Parse/Describe, truy cập HTTP đọc (`/v1/query`, `/v1/get`) khi không lỗi, kết nối replication (cổng `-repl-addr`) và peer Raft.
- Lỗi ghi (đĩa đầy) làm log dừng; máy chủ vẫn chạy (như PostgreSQL khi đích log lỗi). `Logger.Err()` có lỗi đầu tiên; chưa có metric.

## 5. Kiểm chứng

- `go test ./internal/audit`: chuỗi qua xoay tệp và khởi động lại, có/không khoá; 6 kiểu giả mạo; log có khoá cần khoá; dòng cuối dở dang; cắt tỉa tệp cũ và thiếu tệp giữa; truy vấn, phân tích cờ.
- `go test -run 'AuditTrail|AuditObject' ./internal/pgwire`: client pgx thật. Kiểm: đăng nhập đúng/sai mật khẩu/NOLOGIN, ngắt kết nối, CREATE/ALTER/DROP ROLE, GRANT/REVOKE, DDL, từ chối, đọc, ghi, COPY TO/FROM, mật khẩu không có trong tệp (`al1ce-pw`, `second-pw`, `topsecret-bob`...), chuỗi xác minh được; ghi theo đối tượng chỉ ra đúng bảng và thao tác.
- `go test -run ClusterMembersSecurity ./internal/server`: xem docs/13 §8.
- `accept/security.sh` mục "audit trail": staskd thật, 25 lần đăng nhập sai, sửa/xoá/cắt đuôi trên bản sao bị `staskctl audit verify` từ chối.

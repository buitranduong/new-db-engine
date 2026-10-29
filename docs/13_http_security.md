# 13 — Bảo mật API HTTP, pprof và endpoint lỗi giả lập

Hạng mục S2 và S3 của `docs/12_plan.md` §P9. Mã nguồn: `internal/server/auth.go`, nối vào `cmd/staskd/main.go` (hàm `httpSecurity`). Test ở `internal/server/auth_test.go`.

## 1. Hệ thống khác làm thế nào

| Hệ thống | Cách xác thực API HTTP | Điều lấy về cho STASK |
|---|---|---|
| ClickHouse | Basic auth, hoặc header `X-ClickHouse-User`/`X-ClickHouse-Key`. Tham số `password=` trên URL được hỗ trợ nhưng tài liệu khuyên tránh vì proxy ghi log URL ([HTTP interface](https://clickhouse.com/docs/interfaces/http)) | Nhận credential qua header, không bao giờ qua URL. Basic dùng lại mật khẩu sẵn có |
| CockroachDB | Cluster API v2: đăng nhập bằng user/mật khẩu SQL, nhận token phiên, gửi lại trong header `X-Cockroach-API-Session`; thiếu hoặc hết hạn thì 401 ([Cluster API](https://www.cockroachlabs.com/docs/v23.2/cluster-api), [`cockroach auth-session`](https://www.cockroachlabs.com/docs/v24.1/cockroach-auth-session)) | Token mờ (opaque) trong header; 401 khi thiếu. STASK chưa có endpoint đăng nhập: token được cấu hình sẵn (user/role đã có ở PG wire từ 2026-10-29, docs/05 §18, nhưng chưa gắn với HTTP) |
| Elasticsearch | API key gửi trong `Authorization: ApiKey <base64(id:key)>`, có thể giới hạn quyền theo role và đặt hạn; nên dùng một key cho mỗi tích hợp để dễ xoay vòng và thu hồi ([Create API key](https://www.elastic.co/guide/en/elasticsearch/reference/8.17/security-api-create-api-key.html)) | Nhiều token song song (một cho mỗi client), có lớp quyền (đầy đủ / chỉ đọc), xoay vòng không cần tắt server |
| etcd | TLS cho client; `--client-cert-auth` + `--trusted-ca-file` bắt client trình chứng chỉ do CA tin cậy ký, không có thì từ chối ([Transport security model](https://etcd.io/docs/v3.5/op-guide/security/)) | mTLS: chứng chỉ client hợp lệ tự nó là xác thực |
| Prometheus exporter-toolkit | Một file `--web.config.file` cho TLS và basic auth, đọc lại khi file đổi, không cần khởi động lại ([web configuration](https://github.com/prometheus/exporter-toolkit/blob/v0.9.1/docs/web-configuration.md)) | File token đọc lại khi đổi (kiểm mtime mỗi giây) và khi nhận SIGHUP |
| Go `net/http/pprof` | Không có xác thực. Lộ ra ngoài thì rò trạng thái runtime và cho phép đốt CPU (profile CPU dài); cách khuyên dùng là listener riêng chỉ nghe localhost ([Your pprof is showing](https://mmcloughlin.com/posts/your-pprof-is-showing), [golang/go#22085](https://golang.org/issue/22085)) | `-pprof` chỉ được bind loopback trừ khi có `-pprof-insecure` |

Định dạng lỗi theo RFC 6750 (Bearer token): 401 kèm `WWW-Authenticate: Bearer realm="stask"`, thêm `error="invalid_token"` khi token sai, và 403 `error="insufficient_scope"` khi token hợp lệ nhưng không đủ quyền ([RFC 6750 §3](https://www.rfc-editor.org/rfc/rfc6750#section-3)). Basic theo [RFC 7617](https://www.rfc-editor.org/rfc/rfc7617).

## 2. Mô hình đe doạ

- **Kẻ tấn công trên mạng** tới được cổng `-addr`: trước đây đọc/ghi mọi bảng, gọi checkpoint/GC, tạo slot CDC và stream toàn bộ thay đổi. Nay: không có credential thì chỉ gọi được `GET /v1/health`.
- **Nghe lén trên đường truyền**: token Bearer/Basic gửi rõ nếu không có TLS. Vì vậy khi nghe trên địa chỉ không phải loopback, nên bật `-http-tls-cert`. STASK **không bắt buộc** TLS khi có token (để chạy được sau một reverse proxy kết thúc TLS), chỉ bắt buộc phải có xác thực.
- **Đoán token qua thời gian phản hồi**: token được băm SHA-256 lúc nạp; so sánh digest bằng `crypto/subtle.ConstantTimeCompare`, duyệt hết mọi token không dừng sớm, nên thời gian không phụ thuộc nội dung lẫn độ dài token.
- **Token lộ**: thêm token mới vào file, cho client chuyển sang, xoá token cũ; server đọc lại file trong vòng 1 giây (hoặc ngay khi `kill -HUP`). Không cần khởi động lại.
- **Cấu hình sai** (mở `0.0.0.0` mà quên xác thực): staskd từ chối khởi động. Muốn chấp nhận rủi ro phải ghi rõ `-http-insecure`.
- **pprof và lỗi giả lập**: pprof rò trạng thái runtime và cho phép làm chậm server; `POST /v1/admin/fault` làm mọi lần ghi thất bại. pprof chỉ được nghe loopback (trừ `-pprof-insecure`), và khi API có xác thực thì pprof đòi cùng credential (quyền đầy đủ). Endpoint lỗi giả lập nằm dưới `/v1/admin/`, cần token đầy đủ; bật `-enable-faults` luôn in cảnh báo khi khởi động.
- **Ngoài phạm vi**: phân quyền theo bảng cho API HTTP (role chỉ áp dụng ở PG wire, docs/05 §18); chưa giới hạn số lần thử sai (S5); chưa có audit log (S7). Người có quyền đọc file token hoặc file cấu hình trên máy chủ thì có token.

## 3. Cờ

Mọi cờ đều dùng được trong file `-config` (`tên = giá trị`, cờ trên dòng lệnh thắng), vì `internal/conf` áp dụng cho mọi cờ của staskd.

| Cờ | Mặc định | Ý nghĩa |
|---|---|---|
| `-http-token` | rỗng | Danh sách token quyền đầy đủ, cách nhau bằng dấu phẩy |
| `-http-token-ro` | rỗng | Danh sách token chỉ đọc |
| `-http-token-file` | rỗng | File token: mỗi dòng `<token>` hoặc `rw <token>` (đầy đủ), `ro <token>` (chỉ đọc); dòng trống và dòng bắt đầu bằng `#` bỏ qua. Đọc lại khi mtime/kích thước đổi (kiểm mỗi giây) và khi nhận SIGHUP. File lỗi cú pháp hoặc không đọc được thì giữ bộ token cũ và ghi log. File rỗng nghĩa là từ chối mọi người (fail closed) |
| `-http-basic` | false | Nhận thêm HTTP Basic, mật khẩu là `-pg-password`, tên user bất kỳ (chưa có user, như PG wire). Quyền đầy đủ. Bật mà thiếu `-pg-password` thì không khởi động |
| `-http-tls-cert`, `-http-tls-key` | rỗng | Phục vụ API qua HTTPS (tối thiểu TLS 1.2). Phải đặt cùng nhau |
| `-http-tls-client-ca` | rỗng | File PEM các CA: bắt client trình chứng chỉ do CA này ký (mTLS, kiểm ngay lúc bắt tay). Cần `-http-tls-cert`. Nếu không có token nào thì chứng chỉ client chính là xác thực (quyền đầy đủ, như etcd). Nếu có cả token thì cần **cả hai** |
| `-http-insecure` | false | Cho phép `-addr` không phải loopback mà không có xác thực |
| `-pprof-insecure` | false | Cho phép `-pprof` trên địa chỉ không phải loopback |

Ví dụ:

```sh
# nghe trên mạng, TLS, một token ghi và một token chỉ đọc trong file xoay vòng được
cat > /etc/stask/tokens <<'EOF'
# app chính
3c1f...e9
ro 77ab...02
EOF
chmod 600 /etc/stask/tokens
staskd -addr 0.0.0.0:17070 -http-tls-cert server.crt -http-tls-key server.key -http-token-file /etc/stask/tokens
curl --cacert ca.crt -H "Authorization: Bearer 3c1f...e9" https://stask-host:17070/v1/status
staskbench -url https://stask-host:17070 -token 3c1f...e9 ...     # hoặc STASK_HTTP_TOKEN=...

# mTLS thuần
staskd -addr 0.0.0.0:17070 -http-tls-cert server.crt -http-tls-key server.key -http-tls-client-ca clients-ca.pem
curl --cacert ca.crt --cert client.crt --key client.key https://stask-host:17070/v1/status
```

## 4. Hành vi

- **Mặc định không đổi**: không đặt cờ nào thì không có xác thực, `-addr` mặc định `127.0.0.1:17070`. Mọi công cụ trong repo (`staskbench`, `staskmix`, `staskchaos`, `aggbench`, `cdcbench`, `scripts/*.sh`) chạy như cũ. `staskbench` có thêm `-token` (mặc định `$STASK_HTTP_TOKEN`, rỗng = không gửi) để đo một server có token; các công cụ khác tự chạy staskd con trên loopback hoặc gọi server mặc định nên không cần.
- **Kiểm địa chỉ lúc khởi động** (`server.CheckBind`): loopback là IP loopback (`127.0.0.0/8`, `::1`), `localhost`, hoặc tên miền mà *mọi* địa chỉ phân giải ra đều là loopback. Host rỗng (`:17070`), `0.0.0.0`, `[::]`, IP thật, hoặc tên không phân giải được đều tính là không phải loopback. Không phải loopback + không có xác thực (token, Basic hoặc mTLS) + không có `-http-insecure` → `log.Fatal`. TLS một mình **không** tính là xác thực. Cùng quy tắc cho `-pprof` với `-pprof-insecure` (pprof luôn phải loopback, kể cả khi API có token).
- **Header**: `Authorization: Bearer <token>` (scheme không phân biệt hoa thường) hoặc `Authorization: Basic ...` khi bật `-http-basic`. Token không bao giờ được đọc từ URL.
- **Mã trả về**:
  - Không có credential: 401, `WWW-Authenticate: Bearer realm="stask"` (thêm `, Basic realm="stask"` khi bật Basic), thân `{"error":"authentication required"}`.
  - Credential sai hoặc scheme lạ: 401, challenge có thêm `error="invalid_token"`.
  - Token chỉ đọc gọi endpoint ghi/admin: 403, `WWW-Authenticate: Bearer realm="stask", error="insufficient_scope"`.
  - mTLS thiếu hoặc sai chứng chỉ client: bắt tay TLS thất bại, không có phản hồi HTTP.
- **Token chỉ đọc** được gọi: mọi `GET`/`HEAD` (`/v1/tables`, `/v1/get`, `/v1/history`, `/v1/status`, `/v1/cdc`, `/v1/cdc/{slot}/stream`) và `POST /v1/query` (chỉ đọc). Không được: `/v1/write`, `/v1/tables` (POST), `/v1/tx/*` (kể cả `get` trong giao dịch, vì đi qua POST và mở giao dịch giữ snapshot), `/v1/cdc` POST/DELETE, `/v1/cdc/{slot}/ack`, `/v1/admin/*`. Lưu ý: stream CDC với token chỉ đọc vẫn đọc được toàn bộ thay đổi; không xác nhận (ack) được.
- **Health không cần xác thực**: endpoint mới `GET /v1/health` trả `200 {"ok":true}` hoặc `503 {"ok":false}` khi engine đã hỏng ghi. Không lộ tên bảng, số dòng hay lỗi chi tiết. **`/v1/status` cần xác thực** (token chỉ đọc là đủ) vì nó lộ tên bảng, số dòng, định nghĩa view, số giao dịch mở và thông điệp lỗi ghi. Load balancer/healthcheck nên trỏ vào `/v1/health`.
- **pprof**: khi API có xác thực bằng token hoặc Basic, handler pprof đòi cùng credential (quyền đầy đủ). pprof không có TLS riêng (chỉ nghe loopback nên không cần), nên khi API chỉ dùng mTLS thì pprof không gọi được bằng chứng chỉ; dùng thêm một token nếu cần pprof.
- **Cảnh báo khi khởi động**: `-http-insecure` trên địa chỉ không phải loopback, `-pprof-insecure`, và `-enable-faults` đều in dòng `WARNING:`. Dòng `http api: tls=... mtls=... auth=...` cho biết cấu hình đang chạy.

## 5. Kiểm chứng

`go test ./internal/server/` (VM, có `-race`):

- `TestHTTPAuthTokens`: không token → 401 có challenge; token sai, scheme lạ → 401 có `invalid_token`; token đúng (cả hai token trong danh sách, `bearer` chữ thường) → 200; token chỉ đọc: `GET` và `POST /v1/query` 200, ghi/tạo bảng/admin 403 `insufficient_scope`; `/v1/health` 200 không cần token; Basic với `-pg-password` 200, sai mật khẩu 401.
- `TestHTTPAuthDefaultOpen`: không cấu hình gì thì API mở như cũ.
- `TestHTTPAuthTokenFile`: token từ file; ghi đè file → token cũ 401, token mới 200; file lỗi giữ token cũ; file rỗng từ chối mọi người.
- `TestHTTPTLS`: HTTPS với CA tự sinh trong test; gọi HTTP thường vào cổng TLS không được 200.
- `TestHTTPMTLS`: chỉ có CA client: chứng chỉ do CA ký gọi được admin; không có chứng chỉ hoặc chứng chỉ tự ký khác bị từ chối lúc bắt tay.
- `TestBuildErrors`: cert thiếu key, CA client thiếu cert, `-http-basic` thiếu mật khẩu, file token không tồn tại → lỗi khởi động.
- `TestCheckBind`: loopback IPv4/IPv6/`localhost` qua; `:port`, `0.0.0.0`, `[::]`, IP LAN, tên không phân giải được bị từ chối khi không có xác thực; có xác thực hoặc cờ insecure thì qua.

## 6. Giới hạn và hướng xử lý

| Giới hạn | Xử lý |
|---|---|
| Token cấu hình tĩnh, chưa gắn với role SQL, không phân quyền theo bảng | S4 đã làm cho PG wire (docs/05 §18); còn lại: endpoint đăng nhập kiểu CockroachDB và token gắn với role |
| Không giới hạn số lần thử token sai | Hoãn, làm cùng S5 (trễ tăng dần theo IP) |
| Chứng chỉ TLS nạp một lần khi khởi động; đổi chứng chỉ phải restart (file token thì không) | Hoãn: thêm `GetCertificate` đọc lại theo mtime nếu có nhu cầu vận hành thật |
| pprof không có TLS riêng | Không làm: pprof chỉ nghe loopback; cần từ xa thì dùng SSH tunnel |
| `-http-basic` dùng chung mật khẩu PG, chỉ một mật khẩu (không dùng mật khẩu của role) | Basic chỉ là tiện lợi cho curl/công cụ chỉ biết Basic; Basic theo role làm cùng endpoint đăng nhập ở trên |
| Không có audit log cho lời gọi HTTP | Hoãn tới S7 |
| Cổng nhân bản `-repl-addr` vẫn chưa xác thực | Là S1, hạng mục riêng |

## 7. `/metrics` và `/v1/admin/scrub` (2026-10-29)

Hai endpoint mới dùng chung cơ chế xác thực ở trên; chi tiết ở docs/05 §19.

| Endpoint | Quyền | Ghi chú |
|---|---|---|
| `GET /metrics` | token đầy đủ hoặc chỉ đọc (là `GET`); **không** nằm trong danh sách công khai như `/v1/health` | Lộ tên bảng? Không: chỉ có số đếm. Lộ tải, số session, số lần đăng nhập sai, tuổi snapshot cũ nhất. Prometheus cấu hình `authorization: { credentials_file: ... }` (Bearer) trong `scrape_config` |
| `POST /v1/admin/scrub?rate_mb=&wait=&cancel=` | token đầy đủ (là `POST`, token chỉ đọc nhận 403) | Đọc toàn bộ log và checkpoint với tốc độ giới hạn; gây I/O đọc đáng kể |
| `GET /v1/admin/scrub` | token chỉ đọc đủ | Trạng thái; thông báo lỗi có tên file/offset |

Test: `TestMetricsAndScrubEndpoints` (`internal/server/metrics_test.go`) kiểm 401 khi thiếu token, token chỉ đọc đọc được `/metrics` nhưng không bắt đầu được scrub (403), định dạng từng dòng mẫu.

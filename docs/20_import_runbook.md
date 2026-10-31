# 20 — Sổ tay nhập file Excel/CSV hằng ngày (`staskimport`)

Dành cho người vận hành. Công cụ: `cmd/staskimport` (nhánh `histimport`). Mọi con số hiệu năng ở đây là **[chưa đo]**: theo D32 phép đo ở quy mô thật làm ở cuối; tài liệu này chỉ nói công cụ làm gì, đã kiểm chứng gì ở cỡ 50 nghìn dòng, và cách dùng.

## 1. Công cụ làm gì

Đọc một file `.xlsx` hoặc `.csv` (chế độ luồng, từng dòng một, không nạp cả file vào RAM) và áp vào một bảng như **upsert theo khoá**, idempotent:

- dòng chưa có trong bảng: chèn;
- dòng có, khác giá trị: cập nhật;
- dòng có, **giống hệt**: **không ghi gì** (không tạo phiên bản mới, không tăng LSN, không đẩy thêm việc cho CDC, kho lịch sử hay replica);
- ghi **dòng lịch sử cũ → mới chỉ cho dòng chèn/đổi** (bảng lịch sử do bạn chọn trong spec);
- chạy theo **lô nhỏ** (mặc định 1000 dòng), mỗi lô một giao dịch ngắn, khoá theo thứ tự khoá, nên không giữ khoá lâu và không làm các hệ thống Kho/CRM/web chờ nhau (nỗi đau chính trong docs/00 §1);
- có **điểm kiểm tra** (checkpoint) sau mỗi lô: chết giữa chừng thì chạy lại đúng lệnh cũ, nó tiếp tục từ lô cuối đã commit;
- dòng hỏng (sai định dạng, sai giá trị, DB từ chối) **không dừng cả file**: ghi vào file báo lỗi, các dòng còn lại vẫn nhập.

Công cụ nói chuyện với `staskd` qua giao thức PostgreSQL (cổng `-pg-addr`), nên cũng chạy được với PostgreSQL thật khi so sánh.

## 2. Chạy hằng ngày

```
# 1. thử trước, không ghi gì: đếm chèn/đổi/giữ nguyên/lỗi
staskimport -url "postgres://app@host:5432/stask?sslmode=disable" -spec sims.spec.json -dry-run /data/in/2026-11-02.xlsx

# 2. nhập thật
staskimport -url "postgres://..." -spec sims.spec.json /data/in/2026-11-02.xlsx

# 3. nếu bị ngắt (mất mạng, tắt máy, Ctrl-C): chạy LẠI ĐÚNG LỆNH TRÊN, nó tiếp tục
```

URL có thể đặt trong biến môi trường `STASK_URL`. Mật khẩu để trong URL hoặc `PGPASSWORD`, không đưa vào spec.

Cờ chính:

| Cờ | Ý nghĩa |
|---|---|
| `-spec FILE` | spec cột (JSON), xem §3 |
| `-dry-run` | chỉ đọc và đếm; không ghi bảng, không ghi checkpoint |
| `-batch N` | số dòng trong file mỗi giao dịch (mặc định 1000, tối đa 20000). Nhỏ hơn = khoá ngắn hơn, tổng thời gian dài hơn |
| `-pause 50ms` | nghỉ giữa các lô (nhường tải cho giờ cao điểm) |
| `-checkpoint FILE` | đặt tên file checkpoint (mặc định `<file>.import.json` cạnh file nhập) |
| `-errors FILE` | file báo lỗi (mặc định `<file>.errors.csv`) |
| `-restart` | bỏ checkpoint, nhập lại từ dòng 1 (an toàn vì idempotent: dòng đã có thì "giữ nguyên") |
| `-json` | in tổng kết dạng JSON (cho script) |
| `-progress 10s` | dòng tiến độ ra stderr (0 = tắt) |

Mã thoát: `0` xong và không dòng lỗi; `3` xong nhưng có dòng trong file báo lỗi; `1` dừng vì lỗi nặng (kết nối, file/spec sai); `2` sai cách dùng.

## 3. Spec (JSON)

```json
{
  "table": "sims",
  "key": "number",
  "sheet": "Sheet1",
  "columns": [
    {"name": "number",  "source": "SIM",      "pattern": "^0[0-9]{9}$"},
    {"name": "carrier", "source": "Nhà mạng", "enum": ["viettel", "mobifone", "vinaphone"]},
    {"name": "status",  "source": "Trạng thái", "enum": ["new", "paid", "reserved", "sold"],
     "keep_old_when": ["reserved", "sold"]},
    {"name": "price",   "source": "Giá", "type": "int", "min": 0}
  ],
  "touch": {"updated_at": "now"},
  "history": {
    "table": "sim_history", "key_column": "number", "at_column": "changed_at",
    "extra": {"source": "import"}
  }
}
```

- `table`, `key`: bảng và **một** cột khoá (khoá chính hoặc cột duy nhất; khoá ghép chưa hỗ trợ).
- `columns[].source`: tên cột trong dòng tiêu đề của file (không phân biệt hoa thường). Hoặc `"index": 2` (đếm từ 0) cùng `"no_header": true` ở mức spec cho file không có tiêu đề. `skip_rows`: số dòng bỏ qua trước tiêu đề. `delimiter`: dấu phân cách csv (mặc định `,`; `.tsv` mặc định tab).
- `type`: `text` (mặc định), `int`, `float`, `bool`, `timestamp` (nhận RFC3339, `2006-01-02 15:04:05`, `2006-01-02`, `dd/mm/yyyy [hh:mm:ss]`, hoặc số ngày của Excel; hoặc đặt `formats` theo layout Go).
- Kiểm tra mỗi ô: `required` (khoá luôn bắt buộc), `pattern` (regex), `enum` (không phân biệt hoa thường, lưu theo cách viết trong spec), `max_len`, `min`/`max`. Ô trống = NULL.
- `keep_old_when`: nếu dòng **đang có trong bảng** có giá trị cột này thuộc danh sách thì **giữ giá trị cũ** (ví dụ SIM đã `reserved`/`sold` không bị file ngày ghi đè trạng thái). `keep_old_null`: ô trống trong file không ghi đè giá trị cũ.
- `touch`: cột đặt bằng giờ nhập, chỉ cho dòng chèn/đổi (không làm dòng "giữ nguyên" bị coi là đổi).
- `history`: bảng lịch sử phải có sẵn các cột `key_column`, `at_column`, các cột `extra`, và `old_<c>`, `new_<c>` cho mỗi cột theo dõi (mặc định mọi cột trừ khoá; đổi bằng `columns`, `old_prefix`, `new_prefix`). Dòng chèn mới có `old_*` = NULL. Cột `id` riêng của bảng lịch sử dùng `bigserial`. Bỏ `history` nếu không cần: lịch sử cập nhật cũng nằm sẵn trong log của STASK (docs/19).

Spec sai (tên cột lạ, kiểu lạ, regex hỏng, thiếu khoá) bị từ chối trước khi đụng tới dữ liệu. Thiếu cột trong tiêu đề file: dừng ngay, in tiêu đề tìm thấy.

JSON thay vì YAML: tránh thêm phụ thuộc (D16); nếu cần YAML sau này chỉ là một lớp đọc.

## 4. Cách kiểm tra sau khi nhập

1. Tổng kết cuối lệnh (hoặc `-json`): `rows` = số dòng dữ liệu; `inserted + updated + unchanged + invalid + failed + (duplicates)` phải khớp `rows`; `complete: true`.
2. Số dòng lỗi: `wc -l file.errors.csv` (trừ 1 dòng tiêu đề). Cột: `batch,row,column,value,reason`; `row` là số dòng trong file (kể cả tiêu đề). Dòng `failed` là dòng DB từ chối (ví dụ vi phạm NOT NULL), `invalid` là dòng không qua kiểm tra của spec.
3. So đếm trong DB: `SELECT count(*) FROM sims;` và, nếu dùng lịch sử, `SELECT count(*) FROM sim_history WHERE changed_at >= '<giờ bắt đầu>';` phải bằng `inserted + updated` của tổng kết.
4. Mẫu một SIM: `SELECT * FROM stask_history('sims', '0901234567');` (mọi phiên bản trong cửa sổ giữ) hoặc bảng lịch sử riêng.
5. **Chạy lại cùng file với `-restart`** phải cho `unchanged = rows` và 0 chèn/đổi: đó là phép thử idempotent rẻ nhất. (Tồn tại như test tự động: `TestImportCSVIdempotentAndDiff` kiểm LSN không nhúc nhích.)

Lưu ý đếm khi chạy lại sau khi bị ngắt: nếu công cụ chết **sau khi commit lô nhưng trước khi ghi checkpoint**, lô đó được đọc lại và hiện là `unchanged` trong lần chạy tiếp (không ghi gì, không trùng lịch sử); số `inserted/updated` của hai lần cộng lại vẫn đúng, lô ấy chỉ nằm ở cột `unchanged` của lần sau.

## 5. Tiếp tục khi bị ngắt

Checkpoint `<file>.import.json` chứa: dấu vân tay file (kích thước + băm 1 MiB đầu) và spec, số dòng đã xong (`rows_done`), bộ đếm, cờ `complete`. Ghi nguyên tử sau mỗi lô (ghi file tạm, fsync, đổi tên, fsync thư mục).

- Chạy lại cùng lệnh: tiếp từ `rows_done`.
- File hoặc spec **đã đổi** so với checkpoint: dừng với thông báo rõ; dùng `-restart` nếu muốn nhập lại từ đầu (an toàn).
- Checkpoint `complete: true`: lệnh không làm gì, in tổng kết cũ.
- Hỏng checkpoint: xoá file đó hoặc `-restart`.
- Lỗi kết nối / DB đang bận: công cụ thử lại lô với chờ tăng dần (tối đa 8 lần), nếu vẫn lỗi thì dừng và nói dòng nào; chạy lại để tiếp tục. Lỗi do một dòng cụ thể (DB từ chối): lô được tách ra thử từng dòng, dòng hỏng vào file báo lỗi, phần còn lại nhập bình thường.

## 6. Về khoá và tranh chấp

- Mỗi lô: `BEGIN`; đọc các khoá của lô bằng `SELECT .. WHERE khoá IN (..) ORDER BY khoá FOR UPDATE` (khoá theo thứ tự tăng, từng đoạn 250 khoá); so sánh trong chương trình; một câu `INSERT .. VALUES (..),(..) ON CONFLICT (khoá) DO UPDATE` chỉ cho dòng chèn/đổi; một câu chèn lịch sử; `COMMIT`. Giao dịch của người khác chạm cùng khoá chờ tối đa bằng thời gian một lô, và lô khác khoá không bị chặn.
- Xung đột/chết khoá (`40001`, `40P01`, ...): lô được chạy lại tự động.
- Người đọc đồng thời thấy mỗi lô **nguyên vẹn hoặc chưa có** (test `TestConcurrentReadersSeeBatchSnapshots`: 1034 lần đọc `min(price), max(price)` trên đúng khoảng khoá của một lô trong lúc nhập, không lần nào lệch). Cả file **không** là một giao dịch (cố ý: một giao dịch triệu dòng là thứ gây khoá lâu): trong lúc nhập, người đọc có thể thấy một số lô đã mới, số khác còn cũ.
- Tránh `ANY($1)` với mảng trong truy vấn tự viết cho bảng lớn: đo được nó quét bảng và phân tích lại mảng cho mỗi dòng (phát hiện ghi ở báo cáo; công cụ dùng `IN (..)`).
- Đường nhanh của engine: công cụ dùng **upsert nhiều dòng theo lô** (`batch_upsert` của simbench) kết hợp so sánh phía client để **bỏ dòng không đổi**. Đường COPY + MERGE của `simbench` (`copy_merge`) vẫn có ở engine nhưng ghi lại **mọi** dòng; COPY nhị phân chưa hỗ trợ ("COPY BINARY format is not supported"). Việc chọn giữa hai đường ở quy mô triệu dòng thuộc phép đo cuối (D32).

## 7. File .xlsx lớn

`excelize` đọc luồng từng dòng của một sheet, nhưng giữ bảng chuỗi chia sẻ (shared strings) của file; với file triệu dòng nhiều giá trị khác nhau bảng này có thể lớn (excelize tự đổ ra đĩa tạm khi vượt giới hạn giải nén). **[chưa đo ở quy mô triệu dòng]**. Nếu bộ nhớ là vấn đề: xuất file thành `.csv` (đọc hoàn toàn luồng, RAM không đổi theo số dòng) và nhập file csv.

## 8. Giấy phép phụ thuộc mới (D16)

`github.com/xuri/excelize/v2` v2.11.0: **BSD-3-Clause**. Kéo theo: `xuri/efp` (BSD-3), `xuri/nfp` (BSD-3), `richardlehane/mscfb` và `richardlehane/msoleps` (Apache-2.0), `tiendc/go-deepcopy` (MIT), và nâng `golang.org/x/{crypto,net,sync,text}` (BSD-3). Không có GPL/AGPL/BSL/SSPL. Cần người điều phối duyệt khi gộp (AGENT_PROTOCOL §4b).

## 9. Kiểm thử đã có (cỡ 50 nghìn dòng, không đo hiệu năng)

`go test -p 2 ./cmd/staskimport`:

| Test | Chứng minh |
|---|---|
| `TestImportCSVIdempotentAndDiff` | 50k dòng csv: dry-run đếm đúng và không ghi; nhập; nhập lại **không đổi LSN**; ngày 2 với 5000 dòng đổi giá, 500 dòng `sold` giữ trạng thái, 20 dòng sai định dạng vào file báo lỗi, khoá lặp; lịch sử đúng cũ→mới, chỉ cho dòng đổi |
| `TestImportXLSX` | 50k dòng xlsx (tạo bằng excelize StreamWriter), ngày 2 chỉ 500 dòng đổi |
| `TestBadRowDoesNotStopTheFile` | một dòng DB từ chối: `failed=1`, 2999 dòng còn lại vào bảng, khoá dòng hỏng nằm trong báo cáo |
| `TestResumeAfterKill` | tiến trình thật bị `SIGKILL` sau lô thứ 7, ở hai thời điểm (trước/sau khi ghi checkpoint); chạy lại hoàn tất; 50.000 dòng, 50.000 dòng lịch sử, không trùng khoá lịch sử |
| `TestConcurrentReadersSeeBatchSnapshots` | người đọc đồng thời không thấy lô dở; ghi ứng dụng vào khoá khác trong lúc nhập vẫn chạy |
| `TestSpecErrors` | spec/tiêu đề sai bị từ chối |

Thời gian chạy trong test (cả bộ, 8 nhân dùng chung): xem docs/14 Phụ lục X; không dùng làm số hiệu năng.

## 10. Chưa làm / hạn chế

- Khoá ghép (nhiều cột) và nhiều bảng trong một spec.
- YAML (chỉ JSON).
- Đường COPY + staging + so sánh phía server (`merge`) chưa cài: COPY nhị phân không hỗ trợ và đường văn bản cần đo; hiện chỉ có đường upsert theo lô.
- Chưa đo ở triệu dòng: thông lượng, độ trễ giao dịch của người khác trong lúc nhập, bộ nhớ khi đọc xlsx lớn (D32).
- `-pause` là hằng số, chưa tự điều tốc theo độ trễ người dùng khác.
- Nhập từ nhiều tiến trình song song cho cùng file chưa hỗ trợ (checkpoint là tuần tự).

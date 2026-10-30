# AGENT_PROTOCOL — cách làm việc của agent (bắt buộc)

Mục tiêu: nhiều agent làm song song mà **không** thành một mớ hỗn độn. Vấn đề đã xảy ra: agent phản biện chéo thiết kế của nhau, báo cáo "xong" thiếu bằng chứng, nhiều nhánh cùng sửa một gói, đo bằng binary cũ, đĩa đầy. Quy tắc dưới đây giải quyết từng cái.

## 1. Vai trò

- **Người điều phối (orchestrator)**: một phiên duy nhất. Là người **duy nhất** quyết định thiết kế, gộp nhánh vào `main`, sửa `docs/STATUS`, `docs/PROCESS`, `docs/14`, và push. Người điều phối đọc báo cáo agent với thái độ **kiểm chứng, không mặc nhiên tin**.
- **Agent thực thi**: nhận một task có phạm vi hẹp, làm trên nhánh riêng, báo cáo bằng chứng. **Không** quyết định thiết kế ngoài task, **không** gộp, **không** push.
- **Người dùng**: đặt mục tiêu và ưu tiên (docs/15, 16), xác nhận số liệu nghiệp vụ, cấp tài nguyên (máy, quyền). Không phải nhắc lại bối cảnh: bối cảnh nằm trong docs.

## 2. Chống phản biện chéo

1. **Agent không phản biện thiết kế của agent khác và không viết lại phần của nhau.** Thấy vấn đề ngoài task: ghi một dòng vào báo cáo cuối ("phát hiện: ..., bằng chứng: ..."). Người điều phối quyết định xử lý.
2. **Quyết định thiết kế chỉ được ghi ở `docs/PROCESS.md` (nhật ký quyết định) bởi người điều phối.** Agent bám theo quyết định đã ghi; muốn đổi thì nêu đề xuất kèm bằng chứng trong báo cáo, **không tự đổi**.
3. **Đánh giá độc lập** chỉ làm khi người điều phối yêu cầu, với một câu hỏi cụ thể (ví dụ "bản so sánh lưu trữ này có sai số liệu nào không"), và người đánh giá nhận **mã/số liệu**, không nhận kết luận của tác giả.
4. Mâu thuẫn giữa hai tài liệu hay hai báo cáo: **mã và bằng chứng chạy được thắng văn bản**. Ghi chênh lệch vào `docs/14 §3`.

## 2b. Bằng chứng thắng ý kiến; người điều phối tự quyết (bổ sung 2026-10-29)

1. **Bằng chứng trong repo này** (test, `claimcheck`, `pgdiff`, số đo tái lập) thắng mọi nhận định dựa trên kiến thức đã học. Nhận định kiểu "PG/hệ X làm thế này", "thư viện Y tốt hơn", "thiết kế kia sai" mà không kèm bằng chứng chạy được phải gắn nhãn **[chưa kiểm]** và không được dùng để bác một kết quả đã đo hay để đổi thiết kế.
2. **Hai bên mâu thuẫn thì chạy thí nghiệm rẻ**, không tranh luận. Người điều phối giao việc đo; kết quả quyết định; ghi vào docs/14.
3. **Người điều phối tự quyết** mọi việc kỹ thuật, quy trình, thứ tự gộp, dọn dẹp thứ rõ ràng là rác do dự án tạo ra, và push `main` khi `merge-gate` qua. **Chỉ hỏi người dùng** về: (a) tiền hoặc phần cứng, (b) sự thật nghiệp vụ chỉ người dùng biết, (c) hành động phá huỷ ngoài phạm vi dự án.
4. Một quyết định đã ghi trong `docs/PROCESS.md` chỉ đổi khi có **bằng chứng mới**, không vì ý kiến mới.
5. Người điều phối phải nêu **một lập trường rõ ràng** và lý do; không trả lời kiểu "có thể A, có thể B" khi đã có dữ liệu để chọn.

## 3. Quyền sở hữu thư mục (giảm xung đột)

| Khu vực | Quy tắc |
|---|---|
| `internal/sql` (parse, exec, query) | Nhiều nhánh cùng đụng: tối đa **3 nhánh** sửa đồng thời; hook vào file có sẵn **tối thiểu**, logic ở file mới (`<tính năng>.go`) |
| `internal/core` engine/commit | Thận trọng: đổi định dạng đĩa/log phải **dừng và báo cáo**, không tự đổi (docs/09) |
| `internal/proj` | Một nhánh một lúc |
| Gói mới (`internal/<tên>`) | Tự do; nối vào engine sau, qua giao diện nhỏ, mặc định **tắt** |
| `docs/STATUS`, `PROCESS`, `00_START_HERE` | Chỉ người điều phối. Agent chỉ điền hàng của task mình trong `docs/14` |

## 4. Quy trình một task

1. **Brief** (người điều phối viết): mục tiêu, phạm vi, bối cảnh (trỏ docs, không chép lại), tiêu chí xong bằng số, giới hạn tài nguyên, thứ **không** làm.
2. `git worktree add ~/wt/<tên> -b <tên>`; commit thường xuyên lên nhánh; **không sửa `~/stask`, không push**.
3. Build vào thư mục riêng (`go build -o /tmp/<tên>-bin/ ./cmd/...`), **không dùng `bin/`**.
4. Test: `go test -p 1` (hoặc 2); `-race` cho gói đụng tới; cỡ nhỏ.
5. **Điền hàng task trong `docs/14`**: Kế hoạch, Mục tiêu, Thực tế (lệnh tái lập), chênh lệch với docs.
6. **Báo cáo cuối** theo mẫu mục 6.
7. Người điều phối: đọc diff, chạy `scripts/merge-gate.sh` trên nhánh, gộp bằng `git merge`, chạy lại `claimcheck`, cập nhật `docs/STATUS` và `docs/14`, rồi push.

## 4b. Dùng lại trước khi tự viết (quyết định D16)

- Người dùng **không** yêu cầu tự viết mọi thứ. Trước khi viết một thành phần lớn (parser, đồng thuận, lưu trữ, nén, tìm kiếm), agent khảo sát: có thư viện/engine mã nguồn mở phù hợp không, giấy phép gì, ngôn ngữ nào, mức trưởng thành, chi phí tích hợp. Ghi kết quả vào báo cáo.
- Được phép **học thiết kế từ mã công khai** (PostgreSQL, ClickHouse, Lucene, RocksDB, etcd...). Ghi tên nguồn.
- Giấy phép: dự án là MIT. Dùng thư viện **MIT/BSD/Apache-2.0**. **Không** chép hoặc liên kết mã GPL/AGPL/BSL/SSPL. Nghi ngờ thì hỏi người điều phối.
- Thêm phụ thuộc vào `go.mod` phải được người điều phối duyệt.
- Không chắc thành phần có sẵn đáp ứng được thì ghi rõ lý do và bằng chứng, **không** mặc nhiên chọn tự viết.

## 5. Định nghĩa "xong"

Một task chỉ được đánh dấu xong khi có **tất cả**: mã + test đơn vị qua; `-race` qua cho gói đụng tới; có số đo/probe tái lập được ghi trong docs/14; docs liên quan (05/09/12/...) được cập nhật; `merge-gate.sh` qua. Còn thiếu thì dùng đúng nhãn: **Xong-chưa-kiểm** (có mã/test đơn vị, chưa kiểm hệ thống), **Một phần**, **Đang làm**, **Chưa làm**.

## 6. Mẫu báo cáo cuối của agent

```
KẾT QUẢ: <một câu>
Nhánh/commit: <tên>, <hash>
Đã làm: <danh sách file chính>
Bằng chứng: <lệnh chạy> -> <kết quả thật>
Chưa làm / hạn chế: <thành thật, kể cả khi thua PG>
Phát hiện ngoài phạm vi: <nếu có, kèm bằng chứng>
Dọn dẹp: <đã xoá gì>
```

## 7. Tài nguyên và an toàn

- VM chia sẻ: 8 core, 31 GB RAM, đĩa ~57 GB. Kiểm `df -h /` trước khi tạo dữ liệu; **dừng nếu trống < 3 GB**. Không chạy benchmark nặng trong lúc nhiều agent hoạt động (số sẽ sai); benchmark chuẩn cần máy riêng.
- Dùng cổng trong dải riêng của task; không chiếm cổng của người khác (`ss -ltnp` trước). Docker: chỉ container của mình, đặt tên theo task, xoá khi xong.
- **Xoá file**: kiểm tra mục tiêu (`ls`, `du`, tiến trình đang dùng) và **điều kiện dừng thật** (`if` / `set -e`), không chỉ in cảnh báo. Không xoá thứ không do mình tạo trừ khi người điều phối chỉ định. Nếu hệ thống từ chối một hành động, **không tìm đường vòng**; báo cáo.
- Không đặt mật khẩu/khoá vào repo. Commit dùng `git -c user.name=... -c user.email=...`.

## 8. Khi hết giới hạn hoặc đổi agent (tiếp tục không cần người dùng nhắc lại)

1. Đọc theo thứ tự: `docs/00_START_HERE` → `STATUS` → `AGENT_PROTOCOL` → `PROCESS` → `14`.
2. Chạy `git status`, `git branch -vv`, `git worktree list`; đối chiếu với `STATUS §6`.
3. Xem tiến trình nền: `pgrep -af staskd`, `docker ps`, `df -h /`; **đừng giết thứ không nhận ra**.
4. Hàng việc kế tiếp ở `PROCESS §4`. Bắt đầu từ đó.
5. Trước khi kết thúc phiên: cập nhật `STATUS` và `PROCESS`, commit, push.

## §6. Đo hiệu năng độc quyền (D31)
- Mọi phép đo hiệu năng (simbench, simfresh, memmap, pibench, searchbench…) chạy qua `scripts/perf-run.sh <lệnh>`: khoá flock toàn máy, chỉ một job đo một lúc; không bắt đầu khi có cổng/test/build đang chạy hoặc load1 > 1.0; nếu giữa chừng có build/test khác chạy thì kết quả bị đánh dấu INVALID (exit 5) và không được ghi vào docs.
- Agent Làn B không chạy go test/go build nặng trong cửa sổ đo của orchestrator.
- Số đo không đi qua perf-run.sh không được dùng làm bằng chứng hiệu năng.

## §7. Dấu vết cho mọi nhiệm vụ (kể cả của orchestrator)
- Mỗi nhiệm vụ, kể cả tích hợp, đo, dọn dẹp, chép dữ liệu, quyết định quy trình, có một dòng plan-target-actual trong docs/14 (phụ lục của nhánh hoặc Phụ lục U) với bằng chứng tái hiện được. Việc không có dòng này coi là chưa làm.
- Khi một agent báo xong mà thiếu phụ lục, orchestrator bổ sung trước khi gộp.

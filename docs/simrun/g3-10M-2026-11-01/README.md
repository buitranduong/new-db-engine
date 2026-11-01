# G3 10M run, 2026-11-01 (second attempt, snapshot reuse)

Harness commit 6afb539 (engine identical to main d017845). N = 10,000,000 sims (43,000,200 rows). T=4 P=4 R=2, 120 s measured after 30 s warm-up.
Labels are the harness' own: hyb-* DIRTY (load1 at start 1.5-1.8 > 1.0), off-* and pg-* INVALID (r2_page below the 30-sample minimum; PostgreSQL also hit 53100 `/dev/shm` 64 MB limit in Docker and was NOT tuned). `equality` did not run (disk). Single runs, no variance known. Interpretation: docs/14 appendix "G3 10M".

## Thang mức khẳng định (cách được phép nói về lần chạy này)

| Mức | Được nói |
|---|---|
| Đã chứng minh trong điều kiện G3 hiện có | Đường lai R2 chạy p50 khoảng 112 đến 134 ms ở 10M, kể cả khi đang nhập theo lô và nhập một giao dịch lớn; rơi về đường cũ 0; độ trễ follower 0 ms; mọi oracle giữ. Nhập đã chạy thật (112 lô, 560.000 dòng ở `hyb-batched`). |
| Tín hiệu rất mạnh, KHÔNG ghi như tỉ số chính thức | Đường lai nhanh hơn rất nhiều đường hàng của STASK (R2 17 đến 19 s với 11 đến 12 mẫu, lượt INVALID). |
| CHƯA được chứng minh so sánh công bằng | Bất kỳ tỉ số nào so với PostgreSQL ở R2: lượt PostgreSQL INVALID (R2 dưới 30 mẫu, lỗi `/dev/shm` 64 MB của Docker, chưa tinh chỉnh). PostgreSQL đo 7,8 đến 8,5 s chỉ là chỉ báo. Kết luận về giao dịch (T) và R1 dựa trên mẫu lớn (hàng chục nghìn) nhưng vẫn là lượt INVALID/DIRTY, một mẫu mỗi ô. |
| CHƯA được chứng minh end to end | `equality` ở 10M với PostgreSQL từng dòng (dừng vì đĩa); bậc 30M; độ ổn định qua nhiều lần lặp. |

Dung lượng bản chụp (STASK 3.092 MB, PostgreSQL 7.089 MB) là số đo thật nhưng PostgreSQL chưa tinh chỉnh và đo sau khi dừng sạch.


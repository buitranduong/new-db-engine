DROP TABLE IF EXISTS sales;
CREATE TABLE sales (id bigint PRIMARY KEY, region text, product text, channel text, qty int, price float8, ts timestamptz);
-- same distributions as staskbench salesRow(): 16 regions, ~normal product SP0000..SP1999, 4 channels
INSERT INTO sales SELECT i,
  (ARRAY['Ha Noi','Ho Chi Minh','Da Nang','Hai Phong','Can Tho','Hue','Nha Trang','Vinh','Quy Nhon','Buon Ma Thuot','Thai Nguyen','Nam Dinh','Bac Ninh','Vung Tau','Long Xuyen','Rach Gia'])[1 + floor(random()*16)::int],
  'SP' || lpad((abs(floor(sqrt(-2*ln(1-random()))*cos(2*pi()*random())*400))::int % 2000)::text, 4, '0'),
  (ARRAY['web','store','partner','app'])[1 + floor(random()*4)::int],
  1 + floor(random()*20)::int, round((random()*5000)::numeric, 2)::float8,
  to_timestamp(1735689600 + floor(random()*300*86400))
FROM generate_series(0, :n - 1) i;
VACUUM ANALYZE sales;
CHECKPOINT;

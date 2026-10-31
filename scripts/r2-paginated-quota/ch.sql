DROP TABLE IF EXISTS sims;
CREATE TABLE sims (number String, category String, price UInt32, tel String, store_rate UInt8) ENGINE=MergeTree ORDER BY number;
INSERT INTO sims SELECT concat('09', leftPad(toString(number),9,'0')),
  ['TAM_HOA','LOC_PHAT','THAN_TAI','PHONG_THUY','THUONG'][1 + number % 5],
  toUInt32(500000 + (number*7919) % 4000000),
  multiIf(((intDiv(number,5)*13) % 10) < 3, 'A', ((intDiv(number,5)*13) % 10) < 8, 'B', 'C'),
  toUInt8(5 + (number*31) % 6)
FROM numbers(300000);

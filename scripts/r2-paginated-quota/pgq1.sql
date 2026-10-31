SELECT number, tel, price, store_rate FROM (
  SELECT number, tel, price, store_rate,
         row_number() OVER (PARTITION BY tel ORDER BY store_rate DESC, md5(number || '42'), number) AS rn
  FROM sims
  WHERE category = 'TAM_HOA' AND price BETWEEN 1000000 AND 3000000 AND tel IN ('A','B','C')
) t
WHERE (tel='A' AND rn BETWEEN (1-1)*20+1 AND 1*20)
   OR (tel='B' AND rn BETWEEN (1-1)*50+1 AND 1*50)
   OR (tel='C' AND rn BETWEEN (1-1)*30+1 AND 1*30)
ORDER BY store_rate DESC, md5(number || '42'), number;

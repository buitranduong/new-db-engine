SELECT region, count(*), sum(qty) AS sum_qty, avg(price) FROM sales GROUP BY region ORDER BY sum_qty DESC;

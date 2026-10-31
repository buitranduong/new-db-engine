-- Window over an aggregate: daily revenue with a 7-day moving average.
SELECT day, revenue, round(avg(revenue) OVER (ORDER BY day ROWS BETWEEN 6 PRECEDING AND CURRENT ROW)) AS ma7 FROM (
  SELECT date_trunc('day', o.created_at) AS day, sum(o.total) AS revenue
  FROM orders o WHERE o.status IN ('paid', 'shipped') GROUP BY 1
) d ORDER BY day;

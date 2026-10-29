SELECT region, channel, sum(price) FROM sales WHERE product = 'SP0042' GROUP BY region, channel;

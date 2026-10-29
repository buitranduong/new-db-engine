SELECT channel, count(*), max(price) FROM sales WHERE ts >= to_timestamp(1740000000) AND ts < to_timestamp(1745000000) AND qty > 10 GROUP BY channel;

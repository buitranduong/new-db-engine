-- R1 list page by key: available sims of carrier 2 in one category, keyset pagination.
SELECT number, price, store_rate FROM sims
WHERE carrier_id = 2 AND status = 'available' AND category = 'LOC_PHAT' AND number > '09000100000'
ORDER BY number LIMIT 50;

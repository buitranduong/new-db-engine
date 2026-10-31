-- R1 point lookup with the dimension rows (index nested loop shape). Contract: strong.
SELECT s.number, s.price, s.status, c.code, st.name, st.region
FROM sims s JOIN carriers c ON c.id = s.carrier_id JOIN stores st ON st.id = s.store_id
WHERE s.number = '09000012345';

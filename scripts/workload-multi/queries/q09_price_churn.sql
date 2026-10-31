-- History join: sims whose price changed most in the last 7 days of the data, with carrier.
SELECT h.sim_number, c.code, count(*) AS changes, max(h.changed_at) AS last_change
FROM history h JOIN sims s ON s.number = h.sim_number JOIN carriers c ON c.id = s.carrier_id
WHERE h.field = 'price' AND h.changed_at >= timestamptz '2026-10-24 00:00:00+00'
GROUP BY h.sim_number, c.code HAVING count(*) >= 2
ORDER BY changes DESC, h.sim_number LIMIT 50;

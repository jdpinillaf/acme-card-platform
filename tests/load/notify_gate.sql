-- Sprint 7 notifications gate, run after the k6 load (ADR-022). Every row must show 0.
SELECT 'duplicate deliveries (event, target)' AS check, count(*) AS violations FROM (
  SELECT event_id, target FROM notifications.deliveries GROUP BY 1, 2 HAVING count(*) > 1) d
UNION ALL
SELECT 'deliveries pending > 10 min after the load', count(*) FROM notifications.deliveries
  WHERE status = 'pending' AND created_at < now() - interval '10 minutes'
UNION ALL
SELECT 'inbox rows without a delivered delivery', count(*) FROM notifications.inbox i
  LEFT JOIN notifications.deliveries d ON d.id = i.delivery_id AND d.status = 'delivered' WHERE d.id IS NULL
UNION ALL
SELECT 'delivered customer deliveries missing from the inbox', count(*) FROM notifications.deliveries d
  WHERE d.status = 'delivered' AND d.target IN ('push','sms','email') AND NOT EXISTS (SELECT 1 FROM notifications.inbox i WHERE i.delivery_id = d.id)
UNION ALL
SELECT 'events seen twice', count(*) FROM (SELECT event_id FROM notifications.processed_events GROUP BY 1 HAVING count(*) > 1) p
UNION ALL
SELECT 'suspected-fraud alerts more than once per hour', count(*) FROM notifications.fraud_alerts a
  WHERE EXISTS (SELECT 1 FROM notifications.fraud_alerts b WHERE b.account_id = a.account_id AND b.at > a.at AND b.at < a.at + interval '1 hour');

-- Run against one consistent database snapshot together with export_events.sql.
-- DATETIME values deliberately have no timezone: supply the checked storage
-- timezone using train.py --source-timezone UTC (or +08:00, as applicable).
SELECT JSON_OBJECT(
  'video_id', id,
  'author_id', author_id,
  'title', title,
  'description', COALESCE(description, ''),
  'published_at', DATE_FORMAT(published_at, '%Y-%m-%dT%H:%i:%s.%f')
)
FROM video
WHERE status = 2 AND published_at IS NOT NULL
ORDER BY id;

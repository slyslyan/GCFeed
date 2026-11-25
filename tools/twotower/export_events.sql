-- The JOIN matches the published-only catalog; orphan/deleted video events are
-- excluded consistently. This is an operational snapshot, not an archival replay.
SELECT JSON_OBJECT(
  'event_id', ev.id,
  'user_id', ev.user_id,
  'video_id', ev.video_id,
  'event_type', ev.event_type,
  'watch_ms', ev.watch_ms,
  'completed', ev.completed,
  'created_at', DATE_FORMAT(ev.created_at, '%Y-%m-%dT%H:%i:%s.%f')
)
FROM video_view_events AS ev
JOIN video AS v ON v.id = ev.video_id
WHERE v.status = 2 AND v.published_at IS NOT NULL
ORDER BY ev.created_at, ev.id;

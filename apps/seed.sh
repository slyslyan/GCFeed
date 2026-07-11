#!/bin/sh
BASE="http://gcfeed-api:8080"

echo "=== 1. 注册用户 ==="

# user1
curl -s -o /dev/null -w "user1 register: %{http_code}\n" -X POST "$BASE/api/users" \
  -H "Content-Type: application/json" \
  -d '{"account":"alice","password":"alice123","nickname":"Alice"}'

# user2
curl -s -o /dev/null -w "user2 register: %{http_code}\n" -X POST "$BASE/api/users" \
  -H "Content-Type: application/json" \
  -d '{"account":"bob","password":"bob123","nickname":"Bob"}'

echo ""
echo "=== 2. 登录获取 Token ==="

TOKEN1=$(curl -s -X POST "$BASE/api/sessions" \
  -H "Content-Type: application/json" \
  -d '{"account":"alice","password":"alice123"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
echo "Alice token: ${TOKEN1:0:20}..."

TOKEN2=$(curl -s -X POST "$BASE/api/sessions" \
  -H "Content-Type: application/json" \
  -d '{"account":"bob","password":"bob123"}' | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
echo "Bob token: ${TOKEN2:0:20}..."

echo ""
echo "=== 3. 创建视频 ==="

VIDEOS='["https://v3-cdn.oss-cn-hangzhou.aliyuncs.com/sv/1cca3d6a-17b8f458fcf/1cca3d6a-17b8f458fcf.mp4","https://v3-cdn.oss-cn-hangzhou.aliyuncs.com/sv/2d96b7a0-17c01234567/2d96b7a0-17c01234567.mp4","https://v3-cdn.oss-cn-hangzhou.aliyuncs.com/sv/3ea7c8b0-17d0abcdefg/3ea7c8b0-17d0abcdefg.mp4"]'
COVERS='["https://picsum.photos/seed/vid1/640/360","https://picsum.photos/seed/vid2/640/360","https://picsum.photos/seed/vid3/640/360"]'

for i in 1 2 3; do
  MEDIA=$(echo "$VIDEOS" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())[$i-1])")
  COVER=$(echo "$COVERS" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())[$i-1])")
  RESP=$(curl -s -X POST "$BASE/api/videos" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN1" \
    -H "Idempotency-Key: alice-vid-$i" \
    -d "{\"title\":\"Alice 的视频 #$i\",\"description\":\"这是 Alice 的第 $i 个视频\",\"media_url\":\"$MEDIA\",\"cover_url\":\"$COVER\"}")
  VID_ID=$(echo "$RESP" | grep -o '"id":[0-9]*' | cut -d: -f2 | head -1)
  echo "Alice 创建视频 #$i -> ID: $VID_ID"

  # Bob also creates videos
  RESP2=$(curl -s -X POST "$BASE/api/videos" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN2" \
    -H "Idempotency-Key: bob-vid-$i" \
    -d "{\"title\":\"Bob 的视频 #$i\",\"description\":\"这是 Bob 的第 $i 个视频\",\"media_url\":\"$MEDIA\",\"cover_url\":\"$COVER\"}")
  VID_ID2=$(echo "$RESP2" | grep -o '"id":[0-9]*' | cut -d: -f2 | head -1)
  echo "Bob 创建视频 #$i -> ID: $VID_ID2"
done

echo ""
echo "=== 4. Alice 关注 Bob ==="
curl -s -o /dev/null -X PUT "$BASE/api/users/me/following/2" \
  -H "Authorization: Bearer $TOKEN1"

echo ""
echo "=== 5. Bob 给 Alice 的视频点赞 ==="
for vid in $(seq 1 3); do
  curl -s -o /dev/null -X PUT "$BASE/api/videos/$vid/like" \
    -H "Authorization: Bearer $TOKEN2"
done

echo ""
echo "=== 6. Bob 评论 Alice 的视频 ==="
curl -s -o /dev/null -X POST "$BASE/api/videos/1/comments" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN2" \
  -d '{"content":"好视频！"}'

echo ""
echo "=== 完成！==="

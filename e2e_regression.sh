#!/usr/bin/env bash
#
# e2e_regression.sh —— 端到端功能回归
#
# 为什么需要它：此前所有验证都是**逐个组件**做的——单测证明"service 调用了
# repo"，集成测试证明某个事务的 SQL 语义，日志/错误契约靠 AST 门禁与一次性探针。
# 但从来没有一次跑通过完整的用户旅程。这轮重构动了所有 service 构造函数、
# 91 处错误响应、104 处日志、6 处事务位置、GORM logger 与 gin 装配方式，
# 组件都对、拼起来不对是完全可能的，而那种错只有端到端能发现。
#
# 覆盖的链路（每一环都被这轮改动碰过）：
#
#   注册 -> 登录 -> 发布视频 -> 同步视图可见（详情/作者/话题/最新流）
#        -> 关注 -> outbox 消息被 worker 消费 -> 关注流可见   <- 异步链路，最关键
#        -> 点赞/取消（异步落库，最终一致）-> 计数正确、幂等
#        -> 评论 -> @提及 -> 通知落库并可在 /notification/list 读到
#        -> 越权删除他人评论返回 403 而不是 500
#        -> 5xx 不泄漏内部错误文本、4xx 文案可读
#        -> 请求 id 贯穿响应头与日志
#
# 用法：
#   ./e2e_regression.sh                  # 打 docker-compose 起的 127.0.0.1:8080
#   BASE=http://host:8080 ./e2e_regression.sh
#
# 依赖：curl、jq、docker（仅用于直连 MySQL 校验 outbox 与清理限流键）。
# 退出码：0 = 全部通过；1 = 有断言失败。

set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8080}"
API_CT="${API_CT:-feedflow-backend-1}"
DB_CT="${DB_CT:-feedflow-mysql-1}"
REDIS_CT="${REDIS_CT:-feedflow-redis-1}"
DB_USER="${DB_USER:-root}"
DB_PASS="${DB_PASS:-123456}"
DB_NAME="${DB_NAME:-feedflow}"

PASS=0
FAIL=0
FAILED_NAMES=()

pass() { PASS=$((PASS + 1)); printf '  \033[32m✅\033[0m %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); FAILED_NAMES+=("$1"); printf '  \033[31m❌\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# check <描述> <0|1> [补充信息]
check() {
  if [ "$2" = "1" ]; then pass "$1"; else fail "$1" "${3:-}"; fi
}

LAST_STATUS=""
LAST_BODY=""

# post <path> [token] [json]
#
# body 用 `$#` 判断而不是 `${3:-默认}`：后者会把显式传入的空串也当成"没传"，
# 于是"空 body"的用例实际发出去的是 {}，测不到 io.EOF 分类（第一版就错在这）。
post() {
  local path="$1" token="${2:-}"
  local body; if [ $# -ge 3 ]; then body="$3"; else body='{}'; fi
  local args=(-s -X POST "$BASE$path" -H 'Content-Type: application/json'
              --data-raw "$body" -o "/tmp/e2e_body.$$" -w '%{http_code}')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  LAST_STATUS=$(curl "${args[@]}")
  LAST_BODY=$(cat "/tmp/e2e_body.$$" 2>/dev/null)
  rm -f "/tmp/e2e_body.$$"
}

sql() {
  docker exec "$DB_CT" mysql -u"$DB_USER" -p"$DB_PASS" "$DB_NAME" -N -B -e "$1" 2>/dev/null
}

jqr() { jq -r "$1" <<<"$2" 2>/dev/null; }

# json_has_id <body> <id>：递归找任意层级里 .id == <id> 的对象
json_has_id() {
  jq -e --argjson id "$2" '[.. | objects | select(.id? == $id)] | length > 0' <<<"$1" >/dev/null 2>&1
}

# wait_until <秒> <shell 条件>：轮询到条件成立或超时。用于等 MQ 消费。
wait_until() {
  local deadline=$((SECONDS + $1)); shift
  while [ $SECONDS -lt $deadline ]; do
    eval "$1" && return 0
    sleep 0.3
  done
  eval "$1"
}

# log_has / log_has_re：在 API 容器最近的日志里按字面量/正则查找。
#
# 刻意**不用** `grep -q`：`grep -q` 一旦命中就立刻退出，生产端 `docker logs`
# 随即收到 SIGPIPE 并以 141 结束；而本脚本开头设了 `set -o pipefail`，管道
# 退出码会取那个 141，于是"明明找得到"被判成找不到。实测：
#
#   $ set -uo pipefail; docker logs --tail 500 CT | grep -q pat; echo $?
#   141          # PIPESTATUS=141,0 —— grep 成功，docker logs 被 SIGPIPE
#
# 不带 -q 的 grep 会读完全部输入，生产端不会被杀。这个坑把三处日志断言同时
# 变成了恒假/恒真（恒真的检查比没有检查更危险），所以下面还专门加了一节自检。
log_has()    { docker logs --tail 500 "$API_CT" 2>&1 | grep -F -- "$1" >/dev/null; }
log_has_re() { docker logs --tail 500 "$API_CT" 2>&1 | grep -E -- "$1" >/dev/null; }

# redis_cli / timeline_score：直连 Redis 读全局时间线 ZSET。
#
# 为什么必须直接看 ZSET：视频进入时间线的唯一机械证据就是它。消费者
# （outboxworker.go 的 timeline consumer）把 video_id 作为 member 写进
# feed:global_timeline，score 取事件时间。而 /feed/listLatest 在时间线为空或
# 落后时会回退到 DB 重建，/feed/listByFollowing 干脆就是一条 DB 查询——所以
# **只看这两个接口，消费者完全坏掉也照样通过**。第一版就是这么写的，靠停掉
# worker 做变异才发现（详见报告）。
redis_cli()      { docker exec "$REDIS_CT" redis-cli -a "$DB_PASS" --no-auth-warning "$@" 2>/dev/null; }
timeline_score() { redis_cli ZSCORE v1:feed:global_timeline "$VID"; }

# 点赞是 MQ 异步落库的（LikeService 只负责入队，worker 才写 MySQL），
# 所以"点完立刻查 isLiked"必然可能读到旧快照。这是设计如此，不是缺陷——
# 但断言必须等最终一致，否则会得到一个随机红的 flaky 用例。
db_likes_count() { sql "SELECT likes_count FROM videos WHERE id = $VID;"; }
db_like_rows()   { sql "SELECT COUNT(*) FROM likes WHERE video_id = $VID AND account_id = $ID_B;"; }

# ---------------------------------------------------------------------------

section "-1. 断言工具自检（自证检查本身能真能假）"

# 这一节的存在理由：上面 log_has 那个 pipefail/SIGPIPE 的坑，会让日志断言
# 变成永远为真或永远为假，而两种情况下脚本都照样跑完、照样打印 ✅/❌。
# 恒真的检查比没有检查更危险——它会让人以为"这条已经被守住了"。
# 所以先证明工具本身：能找到刚写进去的标记，且不会把不存在的标记说成存在。
selftest_mark="harness-selftest-$$-$RANDOM$RANDOM"
curl -s -o /dev/null -H "X-Request-ID: $selftest_mark" "$BASE/livez"
if wait_until 10 "log_has '$selftest_mark'"; then
  pass "自检：刚写入日志的标记能被 log_has 找到"
else
  fail "自检：刚写入日志的标记能被 log_has 找到" \
       "日志断言工具已失效——它之后的每条日志检查都不可信（见 log_has 的注释）"
fi
check "自检：不存在的标记不会被 log_has 误判为存在" \
  "$(log_has "${selftest_mark}-definitely-absent" && echo 0 || echo 1)"

# 上面两条只覆盖"命中在输出末尾"的情形；而 pipefail/SIGPIPE 那个坑**只在命中
# 出现在输出开头时必然发作**：grep 一命中就退出，生产端还在往管道里写，于是
# 被杀成 141。命中越早，越必然踩到。所以这里专门用一个锚定行首、必然命中的
# 模式再自证一次——这一条才是真正守住那个坑的。
check "自检：命中日志开头的内容时 log_has_re 仍判定为找到（覆盖 grep 提前退出/SIGPIPE）" \
  "$(log_has_re '^20[0-9]{2}-[0-9]{2}-[0-9]{2}T' && echo 1 || echo 0)"

section "0. 部署形态：容器健康 + 新代码确实生效"

for ct in "$API_CT" "$DB_CT" "$REDIS_CT"; do
  st=$(docker inspect -f '{{.State.Health.Status}}' "$ct" 2>/dev/null || echo "missing")
  check "容器 $ct 健康（实际 $st）" "$([ "$st" = "healthy" ] && echo 1 || echo 0)"
done

hdr=$(curl -s -D - -o /dev/null "$BASE/livez")
check "/livez 返回 200" "$(grep -q ' 200 ' <<<"$hdr" && echo 1 || echo 0)"
# X-Request-Id 只有 11e3707 引入的 zap 中间件才会设置；它是"新代码已部署"的判据。
check "响应带 X-Request-Id（证明 zap 中间件已生效）" \
  "$(grep -qi '^x-request-id:' <<<"$hdr" && echo 1 || echo 0)" "$hdr"
check "/readyz 返回 200（依赖都在线）" \
  "$([ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/readyz")" = "200" ] && echo 1 || echo 0)"

# 结构化日志：console 编码器下是一条制表符分隔的记录 + JSON 字段尾。
# 注意默认编码器是 console（config.docker.yaml 未设 log.format），
# 所以这里断言的是"字段结构化"，不是"整行是 JSON"。
logline=$(docker logs --tail 200 "$API_CT" 2>&1 | grep 'request_id' | tail -1)
check "日志已结构化（存在带 request_id 的记录）" "$([ -n "$logline" ] && echo 1 || echo 0)"
for field in '"request_id"' '"method"' '"path"' '"status"' '"latency"'; do
  check "访问日志含字段 $field" "$(grep -q -- "$field" <<<"$logline" && echo 1 || echo 0)" "$logline"
done
# zap 的 console 编码器形如 "2026-10-07T04:15:26.295Z\tINFO\tcaller\tmsg\t{...}"；
# 标准库 log 的默认前缀是 "2026/10/07 12:00:00"，两者不会混淆。
check "日志由 zap 输出（console/JSON 编码器）而不是标准库 log" \
  "$(grep -qE '^(20[0-9]{2}-[0-9]{2}-[0-9]{2}T|\{)' <<<"$logline" && echo 1 || echo 0)" "$logline"
check "无残留的标准库 log 输出（如 '2026/10/07 12:00:00' 前缀）" \
  "$(log_has_re '^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:' && echo 0 || echo 1)"

# ---------------------------------------------------------------------------

section "1. 账号：注册 / 登录 / 错误契约"

# 注册限流是 5 次/小时/IP，反复跑本脚本会撞上；先清掉限流键。
clear_ratelimit() {
  docker exec "$REDIS_CT" redis-cli -a "$DB_PASS" --no-auth-warning \
    --scan --pattern 'feedflow:ratelimit:*' 2>/dev/null \
    | xargs -r docker exec -i "$REDIS_CT" redis-cli -a "$DB_PASS" --no-auth-warning DEL >/dev/null 2>&1
}
clear_ratelimit

TS=$(date +%s)
UA="e2ea$TS"
UB="e2eb$TS"
PW="passw0rd-e2e"

post /account/register "" "{\"username\":\"$UA\",\"password\":\"$PW\"}"
check "注册作者 A 返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"
post /account/register "" "{\"username\":\"$UB\",\"password\":\"$PW\"}"
check "注册观众 B 返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"

# 重名必须是 409 Conflict（#3 错误契约收敛后由 ErrUsernameTaken 给出），
# 而不是 500——这是 91 处错误响应改动里最容易被改回去的一类。
post /account/register "" "{\"username\":\"$UA\",\"password\":\"$PW\"}"
check "重复注册返回 409 而不是 500（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "409" ] && echo 1 || echo 0)" "$LAST_BODY"
check "409 文案可读且不泄漏内部细节" \
  "$(grep -q 'username already exists' <<<"$LAST_BODY" && echo 1 || echo 0)" "$LAST_BODY"

post /account/login "" "{\"username\":\"$UA\",\"password\":\"$PW\"}"
TOKEN_A=$(jqr .token "$LAST_BODY")
ID_A=$(jqr .account_id "$LAST_BODY")
check "作者 A 登录拿到 token" "$([ -n "$TOKEN_A" ] && [ "$TOKEN_A" != "null" ] && echo 1 || echo 0)" "$LAST_BODY"

post /account/login "" "{\"username\":\"$UB\",\"password\":\"$PW\"}"
TOKEN_B=$(jqr .token "$LAST_BODY")
ID_B=$(jqr .account_id "$LAST_BODY")
check "观众 B 登录拿到 token" "$([ -n "$TOKEN_B" ] && [ "$TOKEN_B" != "null" ] && echo 1 || echo 0)" "$LAST_BODY"

post /account/login "" "{\"username\":\"$UA\",\"password\":\"wrong-password\"}"
check "错误密码返回 401 而不是 500（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "401" ] && echo 1 || echo 0)" "$LAST_BODY"

# 真的空 body（不是 {}）走 io.EOF 路径，必须被分类成 400 并给出人话文案。
post /account/login "" ""
check "空请求体返回 400 而不是 500（io.EOF 分类，实际 $LAST_STATUS）" \
  "$([ "$LAST_STATUS" = "400" ] && echo 1 || echo 0)" "$LAST_BODY"
check "空请求体文案可读（不是裸的 'EOF'）" \
  "$(grep -q 'request body is required' <<<"$LAST_BODY" && echo 1 || echo 0)" "$LAST_BODY"

if [ -z "$TOKEN_A" ] || [ "$TOKEN_A" = "null" ]; then
  echo
  echo "作者 A 拿不到 token，后续链路无法继续。响应：$LAST_BODY"
  exit 1
fi

# ---------------------------------------------------------------------------

section "2. 发布视频：写入 + 同步可见性"

TAG="e2etag$TS"
post /video/publish "$TOKEN_A" \
  "{\"title\":\"E2E 视频 #$TAG\",\"description\":\"端到端回归\",\"play_url\":\"http://cdn/e2e.mp4\",\"cover_url\":\"http://cdn/e2e.jpg\"}"
VID=$(jqr '.video.id // .id // .video_id' "$LAST_BODY")
check "发布视频返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"
check "响应带视频 id" "$([ -n "$VID" ] && [ "$VID" != "null" ] && echo 1 || echo 0)" "$LAST_BODY"

if [ -z "$VID" ] || [ "$VID" = "null" ]; then
  echo "拿不到视频 id，终止。响应：$LAST_BODY"
  exit 1
fi

post /video/getDetail "" "{\"id\":$VID}"
check "getDetail 能读到该视频" "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)" "$LAST_BODY"

post /video/listByAuthorID "" "{\"author_id\":$ID_A}"
check "listByAuthorID 包含该视频" "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)"

post /feed/listLatest "" '{"limit":20,"latest_time":0}'
check "最新流（公开）包含该视频" "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)"

# 话题路径走的是本次刚修掉的 resolveTagID（原先的 FirstOrCreate 在并发发布
# 同一新话题时会撞 1062 让整个发布失败）。路由在 feed 组下。
post /feed/listByTag "" "{\"tag_name\":\"$TAG\",\"limit\":10}"
check "listByTag 能按新话题搜到（走 resolveTagID 写入的标签）" \
  "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)" "$LAST_BODY"

# ---------------------------------------------------------------------------

section "3. 异步链路：outbox -> MQ -> 消费者 -> 全局时间线（本轮最关键的一环）"

post /social/follow "$TOKEN_B" "{\"vlogger_id\":$ID_A}"
check "B 关注 A 返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"

# 记录 outbox 行的瞬时状态。
#
# **这里刻意不做断言**：outbox 行是"发布事务写入、轮询消费者取走"的中间态，
# 而轮询间隔是 1s、发布到观察之间只隔几毫秒——行完全可能已经被取走。断言
# "此刻一定有行"会随机红（本脚本确实踩到过一次：单独一项 "发布事务写入了
# outbox 行（实际 0 行）" 失败，而其余 60 项全绿）。
#
# 它的价值是**诊断**而不是判据：真正的判据是"消息最终到达时间线"（下面两条）。
# 当时间线那条失败时，这个数字能区分两种完全不同的故障——
# 有行 = 消费者坏了；从来没有行 = 发布事务坏了。
pending_at_publish=$(sql "SELECT COUNT(*) FROM outbox_msgs WHERE video_id = $VID;")

# 等 outbox 行被消费（轮询间隔 1s，claim 后由消费者删除）。
#
# 注意用词：outbox poller 在 API 与 worker **两个**进程里都会启动
# （internal/http/tasks.go 与 cmd/worker 各起一个），所以这里不能声称
# "由 worker 消费"——实测把 worker 容器停掉，这一段照样通过。
# 要单独验证 worker 就得把它与 API 隔离，那是另一回事。
outbox_left() { sql "SELECT COUNT(*) FROM outbox_msgs WHERE video_id = $VID;"; }
if wait_until 30 '[ "$(outbox_left)" = "0" ]'; then
  pass "outbox 行被消费（30s 内）"
else
  fail "outbox 行被消费（30s 超时，剩 $(outbox_left) 行）" \
       "发布后瞬时观察到 ${pending_at_publish:-?} 行；检查 poller 日志与 timeline 拓扑声明"
fi

# **本节的核心断言**：视频必须作为 member 出现在 Redis 全局时间线 ZSET 里。
# 这是 outbox -> MQ -> 消费者 -> 时间线 整条异步链路唯一无歧义的证据。
# 只看 /feed/listLatest 或 /feed/listByFollowing 都不行：前者在时间线落后时
# 会回退到 DB，后者本来就是 DB 查询，消费者坏掉也照样返回视频。
if wait_until 30 '[ -n "$(timeline_score)" ] && [ "$(timeline_score)" != "nil" ]'; then
  pass "视频进入了 Redis 全局时间线 feed:global_timeline（异步链路端到端成立）"
else
  fail "视频进入了 Redis 全局时间线 feed:global_timeline" \
       "30s 后 ZSCORE 仍为空；发布后瞬时 outbox 行数=${pending_at_publish:-?}（>0 说明消息卡在消费者侧，0 说明发布事务没写 outbox），当前剩 $(outbox_left) 条"
fi

# 时间线有了之后，/feed/listLatest 这个读路径也应能看到（它读的正是这个 ZSET）。
post /feed/listLatest "" '{"limit":20,"latest_time":0}'
check "最新流（读全局时间线）包含该视频" \
  "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)" "$LAST_BODY"
# 关注流（DB 查询）也应包含——这一条验证的是可见性，不是时间线。
post /feed/listByFollowing "$TOKEN_B" '{"limit":20,"latest_time":0}'
check "B 的关注流能看到 A 的新视频" \
  "$(json_has_id "$LAST_BODY" "$VID" && echo 1 || echo 0)" "$LAST_BODY"

# ---------------------------------------------------------------------------

section "4. 点赞：异步落库、计数、幂等"

post /like/like "$TOKEN_B" "{\"video_id\":$VID}"
check "B 点赞返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"

# 点赞经 MQ 异步落库，这里显式等最终一致，并把"异步"本身当作被验证的行为。
if wait_until 20 '[ "$(db_like_rows)" = "1" ]'; then
  pass "点赞行经 MQ 异步落库（最终一致）"
else
  fail "点赞行经 MQ 异步落库" "20s 后 likes 表里仍没有 B 对 $VID 的点赞行"
fi

if wait_until 20 '[ "$(db_likes_count)" = "1" ]'; then
  pass "worker 把 likes_count 加到 1"
else
  fail "worker 把 likes_count 加到 1" "实际 $(db_likes_count)"
fi

post /like/isLiked "$TOKEN_B" "{\"video_id\":$VID}"
check "最终一致后 isLiked = true" "$([ "$(jqr .is_liked "$LAST_BODY")" = "true" ] && echo 1 || echo 0)" "$LAST_BODY"

post /video/getDetail "" "{\"id\":$VID}"
lc=$(jqr '.video.likes_count // .likes_count' "$LAST_BODY")
check "getDetail 的 likes_count = 1（点赞会失效 detail 缓存）" "$([ "$lc" = "1" ] && echo 1 || echo 0)" "$LAST_BODY"

# 重复点赞必须幂等：LikeWithCounts 的唯一键冲突分支靠 errSkipCountUpdate 回滚计数更新。
post /like/like "$TOKEN_B" "{\"video_id\":$VID}"
sleep 2
check "重复点赞幂等，likes_count 仍为 1（实际 $(db_likes_count)）" \
  "$([ "$(db_likes_count)" = "1" ] && echo 1 || echo 0)"
check "重复点赞不产生第二条点赞行（实际 $(db_like_rows)）" \
  "$([ "$(db_like_rows)" = "1" ] && echo 1 || echo 0)"

post /like/unlike "$TOKEN_B" "{\"video_id\":$VID}"
if wait_until 20 '[ "$(db_like_rows)" = "0" ] && [ "$(db_likes_count)" = "0" ]'; then
  pass "取消点赞后点赞行与计数都归零"
else
  fail "取消点赞后点赞行与计数都归零" "行=$(db_like_rows) 计数=$(db_likes_count)"
fi

post /like/unlike "$TOKEN_B" "{\"video_id\":$VID}"
sleep 2
check "重复取消点赞幂等且不变成负数（实际 $(db_likes_count)）" \
  "$([ "$(db_likes_count)" = "0" ] && echo 1 || echo 0)"

# ---------------------------------------------------------------------------

section "5. 评论、@提及通知与越权"

post /comment/publish "$TOKEN_B" "{\"video_id\":$VID,\"content\":\"E2E 评论，提到 @$UA\"}"
check "B 发表评论返回 200（实际 $LAST_STATUS）" "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"

post /comment/listAll "" "{\"video_id\":$VID}"
check "评论列表包含该评论" "$(grep -q 'E2E 评论' <<<"$LAST_BODY" && echo 1 || echo 0)" "$LAST_BODY"

# @提及走 AccountIDByUsername + CreateMentionNotification——这两个下沉到 repo 的
# 方法此前只有 mock 断言，这里第一次被真实执行。
notif=$(sql "SELECT COUNT(*) FROM notifications WHERE recipient_id = $ID_A AND target_id = $VID;")
check "@提及写入了通知（notifications 实际 ${notif:-0} 行）" \
  "$([ "${notif:-0}" -ge 1 ] && echo 1 || echo 0)"

post /notification/unreadCount "$TOKEN_A" '{}'
check "A 的未读数 >= 1" \
  "$([ "$(jqr '.unread_count // .count // 0' "$LAST_BODY")" -ge 1 ] && echo 1 || echo 0)" "$LAST_BODY"

post /notification/list "$TOKEN_A" '{}'
check "通知列表能读到 @提及通知" "$(grep -q '提到' <<<"$LAST_BODY" && echo 1 || echo 0)" "$LAST_BODY"

# 越权：评论由 B 发出，A 不是它的作者，删除必须 403。
# （注意视频本身没有删除路由，VideoService.Delete 目前不可达，见报告。）
COMMENT_B=$(sql "SELECT id FROM comments WHERE video_id = $VID AND author_id = $ID_B ORDER BY id DESC LIMIT 1;")
if [ -n "$COMMENT_B" ]; then
  post /comment/delete "$TOKEN_A" "{\"comment_id\":$COMMENT_B}"
  check "删除他人评论返回 403 而不是 500（实际 $LAST_STATUS）" \
    "$([ "$LAST_STATUS" = "403" ] && echo 1 || echo 0)" "$LAST_BODY"

  post /comment/delete "$TOKEN_B" "{\"comment_id\":$COMMENT_B}"
  check "作者删除自己的评论返回 200（实际 $LAST_STATUS）" \
    "$([ "$LAST_STATUS" = "200" ] && echo 1 || echo 0)" "$LAST_BODY"
else
  fail "能取到 B 的评论 id 以测试越权删除"
fi

# ---------------------------------------------------------------------------

section "6. 错误契约：5xx 不泄漏内部细节"

# 触发一次真实的内部错误：拿一个不存在的视频发评论。
post /comment/publish "$TOKEN_B" '{"video_id":999999999,"content":"x"}'
check "给不存在的视频评论返回 404（实际 $LAST_STATUS）" \
  "$([ "$LAST_STATUS" = "404" ] && echo 1 || echo 0)" "$LAST_BODY"

leaked=0
for needle in "sql:" "gorm" ".go:" "runtime error" "panic" "dial tcp" "Error 1"; do
  grep -qi -- "$needle" <<<"$LAST_BODY" && leaked=1
done
check "错误响应体不含 SQL / GORM / 源码路径等内部痕迹" \
  "$([ "$leaked" = "0" ] && echo 1 || echo 0)" "$LAST_BODY"

post /video/publish "" '{"title":"x","play_url":"y","cover_url":"z"}'
check "未带 token 发布返回 401（实际 $LAST_STATUS）" \
  "$([ "$LAST_STATUS" = "401" ] && echo 1 || echo 0)" "$LAST_BODY"

# ---------------------------------------------------------------------------

section "7. 请求 id 贯穿响应头与日志"

rid=$(curl -s -D - -o /dev/null -H 'X-Request-ID: e2e-trace-12345' "$BASE/livez" \
      | grep -i '^x-request-id:' | tr -d '\r' | awk '{print $2}')
check "透传客户端提供的 X-Request-ID" "$([ "$rid" = "e2e-trace-12345" ] && echo 1 || echo 0)" "实际 $rid"

sleep 1
# 用轮询而不是固定 sleep 1：日志经 Docker 日志驱动落盘有延迟，
# 固定等待偶尔会在写入前就 grep 到（第一版就吃了这个 flake）。
if wait_until 10 "log_has 'e2e-trace-12345'"; then
  pass "该请求 id 出现在后端日志里"
else
  fail "该请求 id 出现在后端日志里" "10s 内未找到 e2e-trace-12345；最后 3 行: $(docker logs --tail 3 "$API_CT" 2>&1 | tr '\n' '|' | cut -c1-500)"
fi

# 恶意 request id 必须被清洗（防日志注入）。
curl -s -o /dev/null -H 'X-Request-ID: bad id with spaces' "$BASE/livez"
sleep 2
check "含空格/非法的 X-Request-ID 被清洗（不会原样进日志）" \
  "$(log_has 'bad id with spaces' && echo 0 || echo 1)"

# ---------------------------------------------------------------------------

section "8. 清理"

# 视频没有删除路由（见报告），直接清库。
sql "DELETE FROM comments WHERE video_id = $VID;" >/dev/null
sql "DELETE FROM notifications WHERE target_id = $VID;" >/dev/null
sql "DELETE FROM video_tags WHERE video_id = $VID;" >/dev/null
sql "DELETE FROM outbox_msgs WHERE video_id = $VID;" >/dev/null
sql "DELETE FROM likes WHERE video_id = $VID;" >/dev/null
redis_cli ZREM v1:feed:global_timeline "$VID" >/dev/null
sql "DELETE FROM videos WHERE id = $VID;" >/dev/null
sql "DELETE FROM tags WHERE name = '$TAG' AND id NOT IN (SELECT tag_id FROM video_tags);" >/dev/null
sql "DELETE FROM follows WHERE follower_id = $ID_B AND vlogger_id = $ID_A;" >/dev/null
sql "DELETE FROM accounts WHERE id IN ($ID_A, $ID_B);" >/dev/null
docker exec "$REDIS_CT" redis-cli -a "$DB_PASS" --no-auth-warning DEL \
  "v1:video:detail:id=$VID" >/dev/null 2>&1
clear_ratelimit
leftover=$(sql "SELECT COUNT(*) FROM videos WHERE id = $VID;")
check "测试数据已清理（视频 $VID 残留 ${leftover:-?} 行）" \
  "$([ "${leftover:-1}" = "0" ] && echo 1 || echo 0)"

# ---------------------------------------------------------------------------

printf '\n\033[1m========== 结果 ==========\033[0m\n'
printf '通过 %d，失败 %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '\n失败项：\n'
  for n in "${FAILED_NAMES[@]}"; do printf '  - %s\n' "$n"; done
  exit 1
fi
printf '\033[32m端到端链路全部通过。\033[0m\n'

#!/usr/bin/env bash
# ==============================================================================
#  verify.sh —— 项目优化点的「体检脚本」  v3
#
#  用途
#    检查代码审计中发现的问题是否真实存在。
#    只做三件事：查索引、看执行计划、grep 代码。
#
#  安全性
#    ★ 全部只读（SHOW / EXPLAIN / grep），没有任何增删改。
#      跑一百遍也不会改变数据库或代码。
#
#  怎么用
#    1. 启动依赖：  docker compose up -d mysql redis rabbitmq
#    2. 仓库根目录： ./verify.sh
#    3. 看每条结论，以及 ❌ / ⚠️ 后面的【怎么修】
#
#  输出怎么看
#    ✅ PASS  没问题（或已修好）
#    ⚠️ WARN  需要你人工判断
#    ❌ FAIL  确认是问题
#
#  ---------------------------------------------------------------------------
#  修订记录
#  v1 → v2：修了 6 处误报（EXPLAIN 解析用了 -N 却按有表头取列、复合索引比对
#           方式错误、死代码把注释行算成引用、把方法定义自身当调用、越权检测
#           过宽、social 索引方向搞反）。
#  v2 → v3：修了残留的 4 处误报 ——
#    1. B3b：原来用 "author_id[[:space:]]*=" 去 grep，命中了 video_entity.go
#       里的 GORM 标签行（`AuthorID uint \`gorm:"index..."\`` 里含 "author_id"）。
#       现改为只匹配 GORM 查询字符串形式："author_id = ?"。
#    2. E2：原来只按"行号范围"过滤，没真正排除方法定义。
#       现用"排除定义行 + 排除已知调用点"的方式判定。
#    3. E3：v2 的正则误匹配了 jwt.GetAccountID 里的 "AccountID" 字样。
#       现直接匹配代码里真实存在的 `session.AccountID !=`。
#    4. 附1：原来硬编码索引名且会因"表不存在"而误报。
#       现改为自动从代码里提取索引名，再去库里核对，并提示可能原因。
#  v3 → v4：收尾轮 ——
#    1. 附1b：新增 11 个关键索引的【列完整性】断言（原附1 只看索引是否存在，
#       因此漏掉了"索引名带 _id 但实际不含 id 列"这类缺陷）
#    2. A1/E1/E2 判据改为三态，能识别"已修复"状态（原先修复后仍报 ❌，共 3 处误报）
#    3. 附1 通过时也写入汇总（原先只有失败时才出现在汇总里）
#
#  ★ 自我校验原则：每条结论都同时打印【依据】（EXPLAIN 原文 / 索引清单 /
#    匹配到的代码行）。你觉得结论可疑时，可以直接看依据判断是脚本错了还是
#    代码错了 —— 不需要相信脚本的判断。
#  ---------------------------------------------------------------------------

set -uo pipefail

# ---------- 配置（与 docker-compose.yml / .env.example 一致）----------
MYSQL_SERVICE="mysql"
MYSQL_USER="root"
MYSQL_PASS="${MYSQL_ROOT_PASSWORD:-123456}"
MYSQL_DB="${MYSQL_DATABASE:-feedflow}"

# ---------- 配色 ----------
if [ -t 1 ]; then
  C_RED=$'\033[31m'; C_GRN=$'\033[32m'; C_YEL=$'\033[33m'
  C_CYN=$'\033[36m'; C_BLD=$'\033[1m'; C_DIM=$'\033[2m'; C_RST=$'\033[0m'
else
  C_RED=""; C_GRN=""; C_YEL=""; C_CYN=""; C_BLD=""; C_DIM=""; C_RST=""
fi

FAIL_COUNT=0
WARN_COUNT=0
SUMMARY=""

section() { echo; echo "${C_BLD}${C_CYN}━━━ $* ━━━${C_RST}"; }
ok()   { echo "  ${C_GRN}✅ PASS${C_RST}  $1"; }
warn() { echo "  ${C_YEL}⚠️  WARN${C_RST}  $1"; WARN_COUNT=$((WARN_COUNT+1)); }
bad()  { echo "  ${C_RED}❌ FAIL${C_RST}  $1"; FAIL_COUNT=$((FAIL_COUNT+1)); }
# 三态输出：用于"缺陷可能已被修复"的检查项。
#   fixed=true  → 缺陷已被修复（✅，且不计入 FAIL）
#   脚本据此区分"仍有问题"与"已经修好"
fixed() { echo "  ${C_GRN}✅ 已修复${C_RST}  $1"; }
note() { echo "        ${C_CYN}→ $*${C_RST}"; }
dim()  { echo "        ${C_DIM}$*${C_RST}"; }
summary_add() { SUMMARY="${SUMMARY}$1|$2\n"; }

# ---------- SQL 工具 ----------
# -B 批量模式(TAB 分隔) + -N 不输出列名 → 只返回数据行
mysql_q() {
  docker compose exec -T "$MYSQL_SERVICE" \
    mysql -u"$MYSQL_USER" -p"$MYSQL_PASS" -D"$MYSQL_DB" -B -N -e "$1" 2>/dev/null
}
mysql_pretty() {
  docker compose exec -T "$MYSQL_SERVICE" \
    mysql -u"$MYSQL_USER" -p"$MYSQL_PASS" -D"$MYSQL_DB" -e "$1" 2>/dev/null
}
table_exists() {
  [ "$(mysql_q "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$MYSQL_DB' AND table_name='$1'")" = "1" ]
}

# MySQL 经典 EXPLAIN 的固定 12 列（因为用了 -N，结果只有 1 行数据）
explain_key()   { mysql_q "EXPLAIN $1" | awk -F'\t' 'NR==1{print $7}'; }
explain_type()  { mysql_q "EXPLAIN $1" | awk -F'\t' 'NR==1{print $5}'; }
explain_rows()  { mysql_q "EXPLAIN $1" | awk -F'\t' 'NR==1{print $10}'; }
explain_extra() { mysql_q "EXPLAIN $1" | awk -F'\t' 'NR==1{print $12}'; }

# 打印 EXPLAIN + 脚本解析到的值（便于分辨"解析错"还是"判断错"）
show_explain() {
  local sql="$1"
  echo "        EXPLAIN 结果："
  mysql_pretty "EXPLAIN $sql" | sed 's/^/          /'
  printf "        %s脚本解析到的值：type=%s  key=%s  rows=%s  Extra=%s%s\n" \
    "$C_DIM" "$(explain_type "$sql")" "$(explain_key "$sql")" \
    "$(explain_rows "$sql")" "$(explain_extra "$sql")" "$C_RST"
}

# 是否存在以给定列**开头**的索引（顺序敏感，允许后面还有更多列）
has_index_starting_with() {
  local table="$1"; shift
  local want seq cols
  want="$(IFS=,; echo "$*")"
  seq=$(mysql_q "SHOW INDEX FROM $table" \
        | awk -F'\t' '$3!="PRIMARY"{print $3"\t"$4"\t"$5}' \
        | sort -k1,1 -k2,2n \
        | awk -F'\t' '{if($1!=p){if(n)print p"|"c; p=$1;c=$3;n=1}else{c=c","$3;n++}} END{if(n)print p"|"c}')
  while IFS='|' read -r _name cols; do
    [ -z "$cols" ] && continue
    case "$cols" in "$want"|"$want",*) return 0 ;; esac
  done <<< "$seq"
  return 1
}

list_indexes() {
  if ! table_exists "$1"; then
    dim "表 $1 不存在（还没被 AutoMigrate 创建）"
    return
  fi
  echo "        该表现有索引："
  mysql_q "SHOW INDEX FROM $1" \
    | awk -F'\t' '$3!="PRIMARY"{print $3"\t"$4"\t"$5}' \
    | sort -k1,1 -k2,2n \
    | awk -F'\t' '{if($1!=p){if(n)printf "          %-36s (%s)\n",p,c; p=$1;c=$3;n=1}else{c=c","$3;n++}} END{if(n)printf "          %-36s (%s)\n",p,c}'
}

# ==============================================================================
echo "${C_BLD}项目优化点体检${C_RST}   $(date '+%Y-%m-%d %H:%M:%S')"
echo "（全部只读检查，不会修改任何数据。建议每改完一项就重跑一次）"

if ! docker compose ps "$MYSQL_SERVICE" 2>/dev/null | grep -qE "Up|running"; then
  echo
  echo "${C_RED}✗ MySQL 容器没有运行。${C_RST}"
  echo "  请先执行： docker compose up -d mysql redis rabbitmq"
  exit 1
fi
if ! mysql_q "SELECT 1" >/dev/null 2>&1; then
  echo
  echo "${C_RED}✗ 连不上 MySQL（$MYSQL_USER@$MYSQL_DB，密码尝试了：$MYSQL_PASS）。${C_RST}"
  exit 1
fi
echo "${C_GRN}✓ MySQL 连接正常${C_RST} (db=$MYSQL_DB, user=$MYSQL_USER)"
dim "库中现有表：$(mysql_q "SHOW TABLES" | paste -sd' ' -)"

# ==============================================================================
section "A1  accounts.refresh_token —— 兜底查询会不会全表扫描？"
# 代码：internal/account/service.go 的 RefreshAccessToken
# 缺陷有两种合法修法：
#   方案A：把 refresh_token 收窄为 varchar 并加索引，然后 WHERE refresh_token=?
#   方案B：删掉 DB 兜底，缓存未命中即视为 token 无效（本项目已采用）
# 因此判据必须看"全表扫描是否消除"，而不是"有没有索引"。

HAS_INDEX="no"
mysql_q "SHOW INDEX FROM accounts" | awk -F'\t' '{print $5}' | grep -qx "refresh_token" && HAS_INDEX="yes"

# 检查是否还有 FindAll 全表加载的调用（排除正则方法 FindAllStringSubmatch）
FINDALL_CALLS=$(grep -rn "as\.FindAll(\|accountRepository\.FindAll(\|ar\.FindAll(" \
                backend/internal/account/ --include=*.go 2>/dev/null)

if [ -z "$FINDALL_CALLS" ]; then
  fixed "A1 已修复：RefreshAccessToken 不再调用 FindAll（全表扫描已消除）"
  if [ "$HAS_INDEX" = "yes" ]; then
    dim "accounts.refresh_token 另有索引（方案A 的痕迹）"
  else
    dim "采用方案B：缓存未命中即返回 invalid refresh token，不加索引（预期行为）"
  fi
  summary_add "A1  refresh_token 全表扫描" "✅ 已修复"
elif [ "$HAS_INDEX" = "yes" ]; then
  warn "仍有 FindAll 调用，但 refresh_token 已有索引"
  note "建议改为按索引查询（WHERE refresh_token = ?），而不是全表加载后线性比对"
  summary_add "A1  refresh_token 全表扫描" "⚠️ 有索引但仍全表加载"
else
  bad "accounts.refresh_token 【没有索引】，且代码仍在调用 FindAll"
  note "依据：$FINDALL_CALLS"
  note "影响：缓存未命中时 SELECT * FROM accounts 把全表加载进内存再逐行比对"
  note "【怎么修】二选一："
  note "  方案A：把 refresh_token 收窄为 varchar(512) 并加索引，改为 WHERE refresh_token=?"
  note "  方案B（更简单，净减代码）：删掉 DB 兜底，缓存未命中即返回 invalid refresh token"
  summary_add "A1  refresh_token 全表扫描" "❌ 仍存在"
fi
dim "当前 accounts 行数：$(mysql_q "SELECT COUNT(*) FROM accounts")（最坏情况下这是要加载到内存的行数）"
list_indexes accounts

# ==============================================================================
section "B1  notifications 未读数 —— 索引够不够？"
# 代码：internal/worker/ssehub.go:199
# 查询：SELECT COUNT(*) FROM notifications WHERE recipient_id=? AND is_read=false

SQL_B1="SELECT COUNT(*) FROM notifications WHERE recipient_id=1 AND is_read=0"
show_explain "$SQL_B1"

if has_index_starting_with notifications recipient_id is_read; then
  ok "存在以 (recipient_id, is_read) 开头的复合索引 —— 过滤和计数都在索引内完成"
  summary_add "B1  notifications 未读数索引" "✅ 已有复合索引"
elif [ "$(explain_type "$SQL_B1")" = "ALL" ]; then
  bad "查询【全表扫描】(type=ALL)"
  summary_add "B1  notifications 未读数索引" "❌ 全表扫描"
else
  warn "走了单列索引（key=$(explain_key "$SQL_B1")），is_read 条件需要回表判断"
  note "说明：现在能按 recipient_id 过滤，数据量小时够用"
  note "【可选优化】加 (recipient_id, is_read) 复合索引可避免回表："
  note '  RecipientID uint `gorm:"index:idx_notif_recipient_read,priority:1;not null"`'
  note '  IsRead      bool `gorm:"index:idx_notif_recipient_read,priority:2;default:false"`'
  note "  notifications 是全项目增长最快的表，数据量上来后收益会变明显。"
  summary_add "B1  notifications 未读数索引" "⚠️ 单列索引（可优化）"
fi
list_indexes notifications

# ==============================================================================
section "B3  comments 评论列表 —— 会不会 filesort？"
# 代码：internal/video/comment_repo.go:28-30
# 查询：WHERE video_id=? ORDER BY created_at ASC LIMIT 200

SQL_B3="SELECT * FROM comments WHERE video_id=1 ORDER BY created_at ASC LIMIT 200"
show_explain "$SQL_B3"

if has_index_starting_with comments video_id created_at; then
  ok "存在以 (video_id, created_at) 开头的复合索引，排序可走索引"
  summary_add "B3  comments 排序索引" "✅ 已有复合索引"
elif echo "$(explain_extra "$SQL_B3")" | grep -qi "filesort"; then
  bad "出现【Using filesort】—— 需要先取出结果再排序"
  note "影响：评论越多越慢；LIMIT 200 是在排序之后才截取的"
  note "【怎么修】把 video_id 单列索引升级为复合索引 (video_id, created_at)："
  note '  VideoID   uint      `gorm:"index:idx_comment_video_created,priority:1"`'
  note '  CreatedAt time.Time `gorm:"index:idx_comment_video_created,priority:2"`'
  summary_add "B3  comments 排序索引" "❌ filesort"
else
  warn "未检测到 filesort，也未找到合适的复合索引（key=$(explain_key "$SQL_B3")）"
  summary_add "B3  comments 排序索引" "⚠️ 需人工判断"
fi
list_indexes comments

# ==============================================================================
# ==============================================================================
section "B3b comments 上有没有【用不到的索引】？"
# 判据：必须同时满足两件事才算"这个索引有用" ——
#   ① 代码里出现 "<col> = ?" 的查询字符串
#   ② 该查询确实是针对 comments 表
#      （而不是恰好同名的 videos.author_id / accounts.username；
#        v3 的早期版本就是在这里放过了这两个索引）
# ★ 依据：comment_repo.go 里操作 comments 表的方式只有三种：
#     Create / Delete(按主键) / Where("video_id = ?") Order("created_at asc")
#   而 Delete 里用到 AuthorID 只是内存里的归属判断，不是查询条件。

UNUSED=""
for col in author_id username; do
  if mysql_q "SHOW INDEX FROM comments" | awk -F'\t' '{print $5}' | grep -qx "$col"; then
    # 针对 comments 表的两种常见写法：
    #   r.db.Model(&Comment{}).Where("author_id = ?", ...)
    #   ...Table("comments").Where("author_id = ?", ...)
    HITS=$(grep -rnE 'Model\(&Comment\{\}\)|Table\("comments"\)' backend/internal/video/ --include=*.go 2>/dev/null \
           | grep "\"$col = ?\"")
    if [ -n "$HITS" ]; then
      ok "comments.$col 有索引，且存在针对 comments 表的该列查询"
      dim "依据：$(echo "$HITS" | head -1)"
    else
      warn "comments.$col 有索引，但【没有针对 comments 表的该列查询】"
      dim "说明：grep 到的同名查询属于别的表（videos.author_id / accounts.username），不算数"
      UNUSED="${UNUSED}${col} "
    fi
  fi
done
if [ -n "$UNUSED" ]; then
  note "影响：每次 INSERT 评论都要多维护这些索引（拖慢写入、多占磁盘）"
  note "【怎么修】确认无用后手动删除（AutoMigrate 只加不删）："
  for c in $UNUSED; do
    real=$(mysql_q "SHOW INDEX FROM comments" | awk -F'\t' -v col="$c" '$5==col{print $3; exit}')
    note "  ALTER TABLE comments DROP INDEX ${real:-idx_comments_$c};"
  done
  summary_add "B3b comments 冗余索引" "⚠️ ${UNUSED}"
else
  ok "没有发现明显的冗余索引"
  summary_add "B3b comments 冗余索引" "✅ 无"
fi

# ==============================================================================
section "B4  video_tags 话题流 JOIN —— 反向索引在不在？"
# 代码：internal/feed/repo.go:107-113
# 先用 tags.name 拿到 tag_id，再按 tag_id 找 video_id → 需要 (tag_id, video_id)

if has_index_starting_with video_tags tag_id video_id; then
  ok "存在以 (tag_id, video_id) 开头的索引"
  summary_add "B4  video_tags 反向索引" "✅ 已有"
else
  bad "【缺少以 (tag_id, video_id) 开头的索引】"
  note "影响：按 tag_id 过滤时无法直接从索引拿到 video_id，需要回表"
  note "【怎么修】给 TagID 补一个复合索引（保留原来两个单列索引不动）："
  note '  VideoID uint `gorm:"index:idx_video_tag_both,priority:1;index;not null"`'
  note '  TagID   uint `gorm:"index:idx_video_tag_both,priority:2;index;not null"`'
  summary_add "B4  video_tags 反向索引" "❌ 缺失"
fi
list_indexes video_tags

# ==============================================================================
section "B5  messages 会话查询 —— 会不会 filesort？"
# 代码：internal/message/handler.go:41-43

SQL_B5="SELECT * FROM messages WHERE (from_id=1 AND to_id=2) OR (from_id=2 AND to_id=1) ORDER BY created_at DESC LIMIT 50"
show_explain "$SQL_B5"

if has_index_starting_with messages from_id to_id created_at; then
  ok "存在以 (from_id, to_id, created_at) 开头的索引，过滤+排序都能走索引"
  summary_add "B5  messages 会话索引" "✅ 已有复合索引"
elif has_index_starting_with messages from_id to_id; then
  warn "有 (from_id, to_id) 复合索引，但缺 created_at，排序仍需 filesort"
  note "【可选优化】扩展为 (from_id, to_id, created_at) 可消除排序"
  summary_add "B5  messages 会话索引" "⚠️ 缺 created_at"
elif echo "$(explain_extra "$SQL_B5")" | grep -qi "filesort"; then
  bad "出现 filesort（当前 key=$(explain_key "$SQL_B5")）"
  note "影响：只有单列索引，OR 条件只能部分命中，排序无法走索引"
  note "【怎么修】把两个单列索引合成一个复合索引："
  note '  FromID    uint      `gorm:"index:idx_message_pair,priority:1;not null"`'
  note '  ToID      uint      `gorm:"index:idx_message_pair,priority:2;not null"`'
  note '  CreatedAt time.Time `gorm:"index:idx_message_pair,priority:3;autoCreateTime"`'
  summary_add "B5  messages 会话索引" "❌ filesort"
else
  warn "未检测到问题（key=$(explain_key "$SQL_B5")）"
  summary_add "B5  messages 会话索引" "⚠️ 需人工判断"
fi
list_indexes messages

# ==============================================================================
section "B6  videos 作者作品列表 —— 会不会 filesort？"
# 代码：internal/video/video_repo.go 的 ListByAuthorID

SQL_B6="SELECT * FROM videos WHERE author_id=1 ORDER BY create_time DESC LIMIT 200"
show_explain "$SQL_B6"

if has_index_starting_with videos author_id create_time; then
  ok "存在以 (author_id, create_time) 开头的索引"
  summary_add "B6  videos 作者列表索引" "✅ 已有"
elif echo "$(explain_extra "$SQL_B6")" | grep -qi "filesort"; then
  bad "出现 filesort（author_id 单列索引不覆盖排序）"
  note "影响：作者主页的作品列表需要额外排序"
  note "【怎么修】给 CreateTime 的索引标签追加一个复合索引（保留现有索引）："
  note '  AuthorID   uint      `gorm:"...;index:idx_videos_author_time,priority:1"`'
  note '  CreateTime time.Time `gorm:"...;index:idx_videos_author_time,priority:2,sort:desc"`'
  summary_add "B6  videos 作者列表索引" "❌ filesort"
else
  warn "未检测到问题（key=$(explain_key "$SQL_B6")）"
  summary_add "B6  videos 作者列表索引" "⚠️ 需人工判断"
fi

# ==============================================================================
section "E1  死代码：dlx.go 的两个符号"
# 状态有三种：
#   (a) 符号已被删除            → 已修复（上一轮就是这样修的）
#   (b) 符号存在但无任何调用     → 死代码，确认问题
#   (c) 符号存在且有调用         → 正常
# 原判据只区分 (b)/(c)，把 (a) 误判为 (b)。

E1_DEAD=""
E1_ALIVE=""
for sym in MaxRetryCount GetRetryCount; do
  # 符号在整个项目里是否还存在（含定义）
  EXISTS=$(grep -rn "$sym" backend/ --include=*.go 2>/dev/null | grep -vc ":[0-9]*:[[:space:]]*//")
  # 除 dlx.go 定义处之外的引用数
  REFS=$( { grep -rn "$sym" backend/ --include=*.go 2>/dev/null \
             | grep -v ":[0-9]*:[[:space:]]*//" | grep -v "dlx.go"; } | wc -l )

  if [ "$EXISTS" -eq 0 ]; then
    dim "$sym：已从代码中删除"
  elif [ "$REFS" -eq 0 ]; then
    dim "$sym：存在但无任何调用（死代码）"
    E1_DEAD="${E1_DEAD}${sym} "
  else
    dim "$sym：被引用 $REFS 处，正常"
    E1_ALIVE="${E1_ALIVE}${sym} "
  fi
done

if [ -n "$E1_ALIVE" ]; then
  ok "符号存在且被引用，无死代码：${E1_ALIVE}"
  summary_add "E1  dlx.go 死代码" "✅ 正常"
elif [ -z "$E1_DEAD" ]; then
  fixed "E1 已修复：MaxRetryCount / GetRetryCount 已从 dlx.go 删除"
  dim "说明：这两个符号是为「基于 x-death 的重试计数」准备的，"
  dim "  但本项目采用【进程内循环重试】，消息从未被 Nack，无使用场景。"
  summary_add "E1  dlx.go 死代码" "✅ 已修复（已删除）"
else
  bad "发现死代码：${E1_DEAD}"
  note "这两个符号（在 dlx.go 中定义）从未被调用"
  note "【怎么修】直接删除符号定义（净减约 20 行）；amqp 的 import 需保留"
  note "  （DeclareDLX 的签名用到 *amqp.Channel）"
  summary_add "E1  dlx.go 死代码" "❌ 存在死代码"
fi

# ==============================================================================
section "E2  死代码：message 模块的 AutoMigrate"
# 状态有三种：
#   (a) 方法已被删除          → 已修复
#   (b) 方法存在但无调用       → 死代码，确认问题
#   (c) 方法存在且有调用       → 正常
# 原判据只区分 (b)/(c)，把 (a) 误判为 (b)。

if ! grep -q "func (r \*Repository) AutoMigrate" backend/internal/message/handler.go 2>/dev/null; then
  fixed "E2 已修复：message.Repository.AutoMigrate 已从 handler.go 删除"
  dim "说明：Message 表的创建由 internal/db/db.go 的统一 AutoMigrate 负责"
  summary_add "E2  message.AutoMigrate 死代码" "✅ 已修复（已删除）"
else
  # 方法还在，检查有无调用点（排除定义行、函数体行、以及 db 包与 notificationworker 的合法用法）
  E2_CALLS=$(grep -rnE "\.AutoMigrate\(" backend/internal/ --include=*.go 2>/dev/null \
             | grep -v "func " \
             | grep -vE "db/db\.go" \
             | grep -vE "notificationworker\.go" \
             | grep -vE "message/handler\.go")
  if [ -z "$E2_CALLS" ]; then
    bad "message.Repository.AutoMigrate 已定义但【无任何调用点】"
    note "依据：全部 AutoMigrate 出现位置如下"
    grep -rn "AutoMigrate" backend/internal/ --include=*.go 2>/dev/null | sed 's/^/          /'
    note "【怎么修】删除 handler.go 中的该 3 行方法定义（净减 3 行）"
    summary_add "E2  message.AutoMigrate 死代码" "❌ 存在死代码"
  else
    ok "AutoMigrate 存在且有调用点"
    summary_add "E2  message.AutoMigrate 死代码" "✅ 正常"
  fi
fi

# ==============================================================================
section "E3  安全：分片上传接口有没有校验会话归属？"
# 代码：internal/video/chunk_handler.go
# 判据：代码里是否出现 `session.AccountID !=` 这样的比较
# ★ v3 修正：v2 的正则误匹配了 jwt.GetAccountID 里的 "AccountID"

CH_ISSUES=""
for fn in ChunkStatus UploadChunk CompleteChunkUpload; do
  BODY=$(awk "/func \\(h \\*ChunkUploadHandler\\) $fn/,/^}/" \
         backend/internal/video/chunk_handler.go 2>/dev/null)
  [ -z "$BODY" ] && continue
  if echo "$BODY" | grep -q "session.AccountID !="; then
    dim "$fn：有归属校验 ✅"
  else
    dim "$fn：没有归属校验 ⚠️"
    CH_ISSUES="${CH_ISSUES}${fn} "
  fi
done
if [ -z "$CH_ISSUES" ]; then
  ok "三个上传接口都校验了 session.AccountID 归属"
  summary_add "E3  分片上传越权" "✅ 已校验归属"
else
  bad "以下接口【没有校验会话归属】：${CH_ISSUES}"
  note "影响：知道别人的 uploadID 就能操作/查看别人的上传会话（IDOR 越权）"
  note "  会话对象里本来就有 AccountID 字段，只是这些接口没用它。"
  note "【怎么修】在 getSession 成功之后加："
  note "  accountID, err := jwt.GetAccountID(c)"
  note "  if err != nil || session.AccountID != accountID {"
  note '      c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"}); return'
  note "  }"
  summary_add "E3  分片上传越权" "❌ 未校验：${CH_ISSUES}"
fi

# ==============================================================================
section "E4  安全/数据：SendMessage 有没有校验接收者存在？"
# 代码：internal/message/handler.go 的 Send

MSG_SEND=$(awk '/func \(h \*Handler\) Send/,/^}/' backend/internal/message/handler.go 2>/dev/null)
if echo "$MSG_SEND" | grep -qE "FindByID|FindByUsername|accountService|accountRepo"; then
  ok "Send 里有查询账号的动作"
  summary_add "E4  SendMessage 校验接收者" "✅ 已校验"
else
  bad "Send 【没有校验 ToID 对应的账号是否存在】"
  note "影响：可以给不存在的用户发私信，产生永久无法投递的孤儿数据"
  note "对比：SocialService.Follow 是双向校验的（social/service.go:24-31）"
  note "【怎么修】在构建 Message 之前加："
  note "  if _, err := accountRepo.FindByID(ctx, req.ToID); err != nil {"
  note '      c.JSON(400, gin.H{"error": "recipient not found"}); return'
  note "  }"
  note "（需要给 message.Service/Handler 注入 account.Repository）"
  summary_add "E4  SendMessage 校验接收者" "❌ 未校验"
fi

# ==============================================================================
section "E5  一致性：删评论有没有减热度？"
# 代码：internal/video/comment_service.go 的 Delete
# 对比：CommentWorker.applyPublish 会 ChangePopularity(+1)；
#       applyDelete 只删评论不动热度 → 热度只增不减

CMT_DEL=$(awk '/func \(s \*CommentService\) Delete/,/^}/' backend/internal/video/comment_service.go 2>/dev/null)
if echo "$CMT_DEL" | grep -q "UpdatePopularityCache"; then
  ok "Delete 里调用了 UpdatePopularityCache"
  summary_add "E5  删评论减热度" "✅ 已处理"
else
  bad "Delete 【没有调用 UpdatePopularityCache】"
  note "影响：热度只增不减（热榜虚高）；详情缓存也不会失效"
  note "【怎么修】在 Delete 返回成功之前加一行（该函数已存在，不用新写）："
  note "  UpdatePopularityCache(ctx, s.cache, comment.VideoID, -1)"
  summary_add "E5  删评论减热度" "❌ 未处理"
fi

# ==============================================================================
section "E6  SSE 有没有写超时保护？"
# 代码：internal/worker/ssehub.go 的 SSEHandler

if grep -q "SetWriteDeadline\|ResponseController" backend/internal/worker/ssehub.go 2>/dev/null; then
  ok "SSEHandler 里有写超时保护"
  summary_add "E6  SSE 写超时" "✅ 已有"
else
  bad "SSEHandler 【没有设置写超时】"
  note "影响：客户端半开连接（拔网线/休眠）时，goroutine + 连接 + channel 永久泄漏"
  note "  原因：这是纯 GET 长连接，服务端不读数据所以检测不到断连；"
  note "        写缓冲满了之后 Fprintf 会永久阻塞，defer Unsubscribe 永不执行。"
  note "【怎么修】每次写之前设写 deadline，写失败就 return："
  note "  rc := http.NewResponseController(c.Writer)"
  note "  _ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))"
  summary_add "E6  SSE 写超时" "❌ 缺失"
fi

# ==============================================================================
section "E7  DelByPattern 用的是 DEL 还是 UNLINK？"
# 代码：internal/middleware/redis/cache.go:34-37

if grep -q "Unlink" backend/internal/middleware/redis/cache.go 2>/dev/null; then
  ok "已使用 UNLINK（异步删除，不阻塞 Redis 主线程）"
  summary_add "E7  DelByPattern 删除方式" "✅ UNLINK"
elif grep -q "rdb.Del" backend/internal/middleware/redis/cache.go 2>/dev/null; then
  warn "用的是同步 DEL，且【逐个删除】"
  note "影响：关注/取关时若匹配到 100 个 key，就是 100 次网络往返（约 100ms）"
  note "【怎么修】二选一："
  note "  1) Del → Unlink（改动最小，语义不变，只是改为异步回收内存）"
  note "  2) 凑批后用 Pipeline 一次发送（RTT 从 100 次降到 1 次）"
  summary_add "E7  DelByPattern 删除方式" "⚠️ 逐个 DEL"
else
  ok "没找到 Del 调用（请人工确认）"
  summary_add "E7  DelByPattern 删除方式" "✅"
fi

# ==============================================================================
section "E8  GetProfile 的 4 个查询是串行还是并发？"
# 代码：internal/http/router.go 的 /account/getProfile 内联 handler

if awk '/accountGroup.POST\("\/getProfile"/,/^\t\}\)/' backend/internal/http/router.go 2>/dev/null \
     | grep -qE "WaitGroup|errgroup|go func"; then
  ok "GetProfile 已经并发化"
  summary_add "E8  GetProfile 并发化" "✅ 已并发"
else
  warn "4 个 Count/Sum 查询是【串行】执行的"
  note "影响：/account/getProfile 延迟 ≈ 4 次数据库往返"
  note "【怎么修】用 errgroup 并发（golang.org/x/sync 已在 go.mod，不新增依赖）"
  note "★ 这一项会改变代码结构，属于需要你确认后才做的类型"
  summary_add "E8  GetProfile 并发化" "⚠️ 串行（可选）"
fi

# ==============================================================================
section "附 1  索引一致性巡检（代码声明 vs 数据库实际）"
# 不硬编码索引名：从代码里提取所有 index:xxx，再去库里核对

CODE_INDEXES=$(grep -rhoE 'index:[a-z_]+' backend/internal/ --include=*.go 2>/dev/null \
               | sed 's/^index://' | sort -u)
MISSING=""
for idx in $CODE_INDEXES; do
  FOUND=$(mysql_q "SELECT COUNT(*) FROM information_schema.statistics
                   WHERE table_schema='$MYSQL_DB' AND index_name='$idx'")
  if [ "$FOUND" = "0" ]; then
    MISSING="${MISSING}${idx} "
  fi
done
if [ -z "$MISSING" ]; then
  ok "代码里声明的索引，数据库里都存在"
  dim "代码中出现的索引名：$(echo $CODE_INDEXES | tr '\n' ' ')"
  summary_add "附1  索引一致性" "✅ 都存在"
else
  warn "以下索引在代码里声明了，但数据库里【不存在】："
  for i in $MISSING; do dim "  $i"; done
  note "可能原因：① 服务启动后没跑过 AutoMigrate；② 对应的表还没创建；③ 索引名拼写不一致"
  note "处理方式：重启 API 服务（启动时会 AutoMigrate），然后重跑本脚本"
  summary_add "附1  索引一致性" "⚠️ 缺失：${MISSING}"
fi

# ---------- 附1b：关键索引的【列完整性】断言 ----------
# 现有逻辑只检查索引是否存在，不检查列是否完整。
# 这里对"名字里承诺了列、且查询依赖这些列"的关键索引做硬编码断言。
# 每个断言格式："表名:索引名:期望的列序列(逗号分隔,顺序敏感)"
KEY_INDEX_ASSERTIONS="
videos:idx_videos_likes_count_id:likes_count,id
videos:idx_videos_popularity_time_id:popularity,create_time,id
videos:idx_videos_author_time:author_id,create_time
videos:idx_videos_create_time:create_time
comments:idx_comment_video_created:video_id,created_at
video_tags:idx_video_tag_tag_video:tag_id,video_id
messages:idx_message_pair:from_id,to_id,created_at
notifications:idx_notif_recipient_read:recipient_id,is_read
socials:idx_social_vlogger_follower:vlogger_id,follower_id
socials:idx_social_follower_vlogger:follower_id,vlogger_id
likes:idx_like_video_account:video_id,account_id
"

INCOMPLETE=""
while IFS=: read -r tbl idx want; do
  [ -z "$tbl" ] && continue
  # 取该索引实际的列序列（按 Seq_in_index 排序后拼接）
  # ★ 这里的 `< /dev/null` 不能省：docker compose exec 会读取并吞掉
  #   while-read 循环的 stdin，导致循环只跑第一轮就结束——那样断言会"恒为真"。
  got=$(mysql_q "SELECT COLUMN_NAME FROM information_schema.statistics
                 WHERE table_schema='$MYSQL_DB' AND table_name='$tbl'
                   AND index_name='$idx' ORDER BY SEQ_IN_INDEX" < /dev/null | paste -sd, -)
  if [ -z "$got" ]; then
    INCOMPLETE="${INCOMPLETE}${tbl}.${idx}(缺失) "
  elif [ "$got" != "$want" ]; then
    INCOMPLETE="${INCOMPLETE}${tbl}.${idx}(实际:${got}|期望:${want}) "
  else
    dim "$tbl.$idx 列完整：($got)"
  fi
done <<< "$KEY_INDEX_ASSERTIONS"

if [ -z "$INCOMPLETE" ]; then
  ok "关键索引的列结构全部符合预期"
  summary_add "附1b 关键索引列完整性" "✅ 全部正确"
else
  bad "以下关键索引的列结构不符合预期："
  for i in $INCOMPLETE; do dim "  $i"; done
  note "影响：索引缺少排序列会导致查询出现 Using filesort（见各项 EXPLAIN）"
  note "【怎么修】确认 struct tag 的 priority 是否覆盖了全部需要的列，"
  note "  然后重启服务让 AutoMigrate 重建；若 GORM 未重建，手工执行："
  note "  ALTER TABLE <表> DROP INDEX <索引>;"
  note "  ALTER TABLE <表> ADD INDEX <索引> (<期望的列序列>);"
  summary_add "附1b 关键索引列完整性" "❌ 不符合：${INCOMPLETE}"
fi

# ==============================================================================
section "附 2  socials 表索引方向核查"
# 粉丝列表：JOIN accounts ON accounts.id=socials.follower_id WHERE socials.vlogger_id=?
#   → 理想索引 (vlogger_id, follower_id)
# 关注列表：JOIN accounts ON accounts.id=socials.vlogger_id WHERE socials.follower_id=?
#   → 理想索引 (follower_id, vlogger_id)
list_indexes socials

if ! table_exists socials; then
  dim "表 socials 不存在，跳过（重启 API 服务会自动创建）"
  summary_add "附2  socials 索引方向" "⏭️ 表不存在"
elif has_index_starting_with socials vlogger_id follower_id; then
  ok "有 (vlogger_id, follower_id) —— 粉丝列表 + CountFollowers 都能走索引"
  summary_add "附2  socials 粉丝方向索引" "✅ 已有"
else
  warn "缺少 (vlogger_id, follower_id) 索引"
  note "影响：粉丝列表 JOIN 与 CountFollowers 只能用到 vlogger_id 单列索引，取 follower_id 要回表"
  note "  现有 uniqueIndex 是 (follower_id, vlogger_id)，方向相反，按最左前缀原则帮不上忙"
  note "【怎么修】给 VloggerID 补一个复合索引："
  note '  VloggerID  uint `gorm:"...;index:idx_social_vlogger_follower,priority:1;uniqueIndex:idx_social_follower_vlogger"`'
  note '  FollowerID uint `gorm:"...;index:idx_social_vlogger_follower,priority:2;uniqueIndex:idx_social_follower_vlogger"`'
  summary_add "附2  socials 粉丝方向索引" "⚠️ 缺反向复合索引"
fi

# ==============================================================================
echo
echo "${C_BLD}${C_CYN}━━━ 汇总 ━━━${C_RST}"
echo
printf "  %-36s %s\n" "检查项" "结论"
printf "  %s\n" "──────────────────────────────────────────────────────────────"
echo -e "$SUMMARY" | sed '/^$/d' | while IFS='|' read -r name status; do
  printf "  %-36s %s\n" "$name" "$status"
done
echo
echo "  ${C_YEL}需你判断${C_RST}：${WARN_COUNT} 项      ${C_RED}确认问题${C_RST}：${FAIL_COUNT} 项"
echo
if [ "$FAIL_COUNT" -gt 0 ]; then
  echo "  ${C_BLD}建议：从「确认问题」里挑【你最看得懂的那一项】开始。${C_RST}"
  echo "        每改一项：git commit → 重跑本脚本 → 看这一项是否变成 ✅"
else
  echo "  ${C_GRN}没有发现确认的问题。${C_RST}"
fi
echo
echo "  ${C_CYN}本脚本只读，可随时重跑。v4 新增附1b 列完整性断言 + A1/E1/E2 三态判据。${C_RST}"
echo

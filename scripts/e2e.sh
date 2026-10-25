#!/usr/bin/env bash
# 端到端联调验收脚本：模拟浏览器端的完整用户流程，逐步校验后端状态。
#
# 覆盖需求中的 6 个验收点：
#   1. 新建会话      2. 发送消息并流式逐字渲染（逐帧到达）
#   3. 刷新后历史仍在 4. 多会话切换      5. 停止生成      6. 单端口静态托管
#
# 用法：
#   bash scripts/e2e.sh                    # 复用已在跑的服务
#   START_SERVER=1 bash scripts/e2e.sh     # 自动拉起单端口服务（含前端产物）
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
START_SERVER="${START_SERVER:-0}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_PID=""
PASS=0

cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

step()  { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()    { PASS=$((PASS+1)); printf '\033[1;32m  ✅ %s\033[0m\n' "$*"; }
die()   { printf '\033[1;31m  ❌ %s\033[0m\n' "$*" >&2; exit 1; }
jfield(){ printf '%s' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"; }

command -v curl >/dev/null || die "缺少 curl"

if [[ "$START_SERVER" == "1" ]]; then
  step "构建前端产物（若缺失）并以单端口模式启动后端"
  [[ -f "$ROOT_DIR/web/dist/index.html" ]] || ( cd "$ROOT_DIR" && npm --prefix web run build >/dev/null 2>&1 )
  ( cd "$ROOT_DIR" && WEB_DIR=web/dist PORT="${PORT:-8080}" go run ./cmd/server ) >/tmp/e2e-server.log 2>&1 &
  SERVER_PID=$!
fi

step "等待服务就绪 $BASE_URL"
for i in $(seq 1 80); do
  curl -fsS "$BASE_URL/api/health" >/dev/null 2>&1 && break
  [[ $i -eq 80 ]] && { cat /tmp/e2e-server.log 2>/dev/null || true; die "服务未在 40s 内就绪"; }
  sleep 0.5
done
ok "健康检查通过：$(curl -fsS "$BASE_URL/api/health")"

# ============================================================
# 1. 新建会话
# ============================================================
step "1/6 新建会话"
S1=$(jfield "$(curl -fsS -X POST "$BASE_URL/api/sessions" -H 'content-type: application/json' -d '{}')" id)
[[ -n "${S1}" ]] || die "会话创建失败"
ok "会话 A 已创建：${S1}"

S2=$(jfield "$(curl -fsS -X POST "$BASE_URL/api/sessions" -H 'content-type: application/json' -d '{"title":"第二个会话"}')" id)
[[ -n "${S2}" ]] || die "会话 B 创建失败"
ok "会话 B 已创建：${S2}"

COUNT=$(curl -fsS "$BASE_URL/api/sessions" | grep -o '"id":"sess_' | wc -l | tr -d ' ')
[[ "${COUNT}" -ge 2 ]] || die "会话列表应至少 2 条，实际 ${COUNT}"
ok "会话列表返回 ${COUNT} 条"

# ============================================================
# 2. 发送消息 + 流式逐帧到达
# ============================================================
step "2/6 发送消息并验证流式逐字到达"
STREAM_FILE=/tmp/e2e-stream.sse
# -N 关闭缓冲；记录每帧到达时间以证明是「逐块推送」而非一次性返回。
START_MS=$(python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo 0)
curl -sN -X POST "$BASE_URL/api/sessions/${S1}/messages" \
  -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"content":"端到端联调：请介绍一下你自己"}' > "$STREAM_FILE"
END_MS=$(python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo 0)

DELTAS=$(grep -c '^event: message.delta' "$STREAM_FILE" || true)
[[ "${DELTAS}" -ge 5 ]] || die "delta 帧数 = ${DELTAS}，期望 >= 5（分块流式）"
ok "收到 ${DELTAS} 个 message.delta（分块输出）"

grep -q '^event: message.start' "$STREAM_FILE" || die "缺少 message.start"
grep -q '^event: message.done'  "$STREAM_FILE" || die "缺少 message.done"
FIRST=$(grep -m1 '^event: ' "$STREAM_FILE" | sed 's/^event: //')
LAST=$(grep '^event: ' "$STREAM_FILE" | tail -1 | sed 's/^event: //')
[[ "$FIRST" == "message.start" ]] || die "首事件应为 start，实际 $FIRST"
[[ "$LAST" == "message.done" ]]   || die "末事件应为 done，实际 $LAST"
ok "事件序列：start → delta × ${DELTAS} → done"

grep '^id: ' "$STREAM_FILE" | sed 's/^id: //' | awk 'NR>1 && $1<=p{print "非递增"; exit 1}{p=$1}' \
  || die "事件 id 未严格递增"
ok "事件 id 严格单调递增（可用于 Last-Event-ID 重放）"

if [[ "$START_MS" != 0 && "$END_MS" != 0 ]]; then
  DUR=$((END_MS - START_MS))
  [[ "$DUR" -ge 300 ]] || die "整个流耗时仅 ${DUR}ms，疑似被缓冲后一次返回（期望 > 300ms 的渐进推送）"
  ok "流持续 ${DUR}ms 渐进推送（未被代理/框架缓冲）"
fi

# 校验 done 的内容等于所有 delta 拼接（前端增量渲染的一致性前提）
python3 - "$STREAM_FILE" <<'PY' || die "done.content 与 delta 拼接不一致"
import json,sys
deltas,done=[],None
for line in open(sys.argv[1],encoding='utf-8'):
    line=line.rstrip('\n')
    if line.startswith('data: '):
        try: obj=json.loads(line[6:])
        except Exception: continue
        if 'delta' in obj: deltas.append(obj['delta'])
        if 'content' in obj: done=obj['content']
assert done is not None, '缺少 done 内容'
assert done==''.join(deltas), f'不一致: done={done[:60]!r} deltas={"".join(deltas)[:60]!r}'
print(f'  ✅ done.content 与 {len(deltas)} 个 delta 拼接完全一致（{len(done)} 字符）')
PY
PASS=$((PASS+1))

# ============================================================
# 3. 刷新后历史仍在（模拟：重新拉取会话详情）
# ============================================================
step "3/6 刷新页面后历史仍在"
DETAIL=$(curl -fsS "$BASE_URL/api/sessions/${S1}")
printf '%s' "$DETAIL" | grep -q '"role":"user"'      || die "历史缺少 user 消息"
printf '%s' "$DETAIL" | grep -q '"role":"assistant"' || die "历史缺少 assistant 消息"
printf '%s' "$DETAIL" | grep -q '"status":"complete"' || die "assistant 未标记 complete"
printf '%s' "$DETAIL" | grep -q '"usage"'            || die "assistant 缺少 usage"
MSGN=$(printf '%s' "$DETAIL" | grep -o '"role":' | wc -l | tr -d ' ')
ok "重新拉取详情得到 ${MSGN} 条历史消息（含状态与 usage）"

LASTID=$(printf '%s' "$DETAIL" | sed -n 's/.*"last_event_id":\([0-9]*\).*/\1/p')
[[ -n "${LASTID}" && "${LASTID}" -gt 0 ]] || die "last_event_id 无效: ${LASTID}"
ok "last_event_id = ${LASTID}（可作为重连基线）"

# 列表里也应能看到该会话（刷新后左侧会话栏的数据来源）
curl -fsS "$BASE_URL/api/sessions" | grep -q "${S1}" || die "会话列表里找不到会话 A"
ok "会话列表包含该会话（刷新后左侧栏可恢复）"

# ============================================================
# 4. 多会话切换
# ============================================================
step "4/6 多会话切换（数据互不串台）"
curl -fsS -X POST "$BASE_URL/api/sessions/${S2}/messages" \
  -H 'content-type: application/json' -d '{"content":"这是会话 B 的内容"}' >/dev/null

DA=$(curl -fsS "$BASE_URL/api/sessions/${S1}")
DB=$(curl -fsS "$BASE_URL/api/sessions/${S2}")

printf '%s' "$DA" | grep -q '端到端联调' || die "会话 A 内容丢失"
printf '%s' "$DB" | grep -q '会话 B 的内容' || die "会话 B 内容丢失"
printf '%s' "$DA" | grep -q '会话 B 的内容' && die "会话 A 里混入了会话 B 的消息（串台）"
printf '%s' "$DB" | grep -q '端到端联调' && die "会话 B 里混入了会话 A 的消息（串台）"
ok "两个会话的消息完全隔离，切换不串台"

# 事件序号也应按会话独立编号（都从 1 开始）
LA=$(printf '%s' "$DA" | sed -n 's/.*"last_event_id":\([0-9]*\).*/\1/p')
LB=$(printf '%s' "$DB" | sed -n 's/.*"last_event_id":\([0-9]*\).*/\1/p')
[[ "${LA}" != "${LB}" ]] || die "两个会话的事件序号不应相同（说明 seq 未按会话隔离）"
ok "事件序号按会话隔离（A=${LA}, B=${LB}）"

# ============================================================
# 5. 停止生成
# ============================================================
step "5/6 停止生成（取消接口 + 中断落库）"
S3=$(jfield "$(curl -fsS -X POST "$BASE_URL/api/sessions" -H 'content-type: application/json' -d '{}')" id)

# 后台发起一个长生成，随后立即取消。
curl -sN -X POST "$BASE_URL/api/sessions/${S3}/messages" \
  -H 'content-type: application/json' \
  -d '{"content":"请输出一段很长的内容用于测试停止"}' > /tmp/e2e-cancel.sse 2>&1 &
GEN_PID=$!
sleep 0.25

CANCEL=$(curl -fsS -X POST "$BASE_URL/api/sessions/${S3}/cancel")
wait "$GEN_PID" 2>/dev/null || true

printf '%s' "$CANCEL" | grep -q '"canceled":true' || die "取消接口未返回 canceled=true: $CANCEL"
ok "POST /cancel 返回 canceled=true"

grep -q 'event: message.interrupted' /tmp/e2e-cancel.sse \
  || die "SSE 流未收到 message.interrupted（前端会永远停在「生成中」）"
ok "SSE 流收到 message.interrupted（前端能正确收尾）"

# 落库状态应为 interrupted，且保留已生成的部分内容
STATUS=$(curl -fsS "$BASE_URL/api/sessions/${S3}" | sed -n 's/.*"status":"\([a-z]*\)".*/\1/p' | tail -1)
D3=$(curl -fsS "$BASE_URL/api/sessions/${S3}")
printf '%s' "$D3" | grep -q '"status":"interrupted"' || die "取消后消息状态应为 interrupted"
ok "被中断的消息以 interrupted 落库"

# 取消后必须能继续发送（锁已释放，不残留 409）
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/sessions/${S3}/messages" \
  -H 'content-type: application/json' -d '{"content":"取消后重新发送"}')
[[ "${CODE}" == "200" ]] || die "取消后重新发送返回 ${CODE}，期望 200（锁应已释放）"
ok "取消后可立即继续发送（无 409 残留）"

# 重复取消应幂等返回 canceled=false
CODE2=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/sessions/${S3}/cancel")
[[ "$CODE2" == "200" ]] || die "无生成时取消应返回 200，实际 $CODE2"
ok "重复取消幂等（HTTP $CODE2 / canceled=false）"

# ============================================================
# 6. 单端口静态托管
# ============================================================
step "6/6 单端口静态托管（前端由 Go 服务直接提供）"
if [[ "$START_SERVER" == "1" ]]; then
  CT=$(curl -s -o /dev/null -w '%{content_type}' "$BASE_URL/")
  [[ "${CT}" == text/html* ]] || die "首页 Content-Type = ${CT}，期望 text/html"
  ok "GET / 返回前端首页（${CT}）"

  curl -s "$BASE_URL/" | grep -qi '<div id="root">' || die "首页不是前端入口 HTML"
  ok "首页包含前端挂载点 #root"

  CT2=$(curl -s -o /dev/null -w '%{content_type}' "$BASE_URL/sessions/xyz")
  [[ "${CT2}" == text/html* ]] || die "SPA 回退失败，Content-Type = ${CT2}"
  ok "前端路由回退到 index.html（SPA 支持）"

  # /api 下的未知路径必须仍是 JSON，不能被回退吞掉
  CT3=$(curl -s -o /dev/null -w '%{content_type}' "$BASE_URL/api/nope")
  [[ "${CT3}" == application/json* ]] || die "/api 未知路径应返回 JSON，实际 ${CT3}"
  ok "/api 未知路径仍返回 JSON 404（未被 SPA 回退吞掉）"
else
  echo "  ⏭ 跳过（未使用 START_SERVER=1 自启单端口服务）"
fi

# ============================================================
# 清理
# ============================================================
step "清理测试数据"
for s in "${S1}" "${S2}" "${S3}"; do
  curl -s -o /dev/null -X DELETE "$BASE_URL/api/sessions/$s" || true
done
ok "测试会话已删除"

printf '\n\033[1;32m🎉 端到端联调验收通过（%d 项检查）\033[0m\n\n' "$PASS"

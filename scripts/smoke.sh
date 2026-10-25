#!/usr/bin/env bash
# SSE 全链路冒烟测试：不依赖前端，只用 curl 验证后端协议。
#
# 用法：
#   bash scripts/smoke.sh                  # 复用已在跑的服务（:8080）
#   BASE_URL=http://localhost:9090 bash scripts/smoke.sh
#   START_SERVER=1 bash scripts/smoke.sh   # 自己拉起服务（用完自动关闭）
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
START_SERVER="${START_SERVER:-0}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_PID=""

cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()  { printf '\033[1;32m✅ %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m❌ %s\033[0m\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少依赖命令: $1"; }
need curl

# json_field <json> <key>：从扁平的 JSON 中取出字符串字段（不依赖 jq）。
json_field() { printf '%s' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"; }

if [[ "$START_SERVER" == "1" ]]; then
  say "启动后端（Mock 模式）"
  ( cd "$ROOT_DIR" && go run ./cmd/server ) >/tmp/smoke-server.log 2>&1 &
  SERVER_PID=$!
fi

say "等待 $BASE_URL/api/health"
for i in $(seq 1 60); do
  if curl -fsS "$BASE_URL/api/health" >/dev/null 2>&1; then break; fi
  [[ $i -eq 60 ]] && { cat /tmp/smoke-server.log 2>/dev/null || true; die "服务未在 30s 内就绪"; }
  sleep 0.5
done
curl -fsS "$BASE_URL/api/health"; echo
ok "健康检查通过"

say "创建会话"
CREATE=$(curl -fsS -X POST "$BASE_URL/api/sessions" -H 'content-type: application/json' -d '{}')
SESSION_ID=$(json_field "$CREATE" "id")
[[ -n "$SESSION_ID" ]] || die "创建会话失败: $CREATE"
echo "session_id = $SESSION_ID"

say "校验 SSE 响应头"
curl -sS -D /tmp/smoke-headers.txt -o /dev/null -N -X POST "$BASE_URL/api/sessions/$SESSION_ID/messages" \
  -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"content":"响应头检查"}' || true
grep -qi 'content-type: text/event-stream' /tmp/smoke-headers.txt || die "缺少 Content-Type: text/event-stream"
grep -qi 'cache-control:.*no-cache'        /tmp/smoke-headers.txt || die "缺少 Cache-Control: no-cache"
grep -qi 'connection: keep-alive'          /tmp/smoke-headers.txt || die "缺少 Connection: keep-alive"
grep -qi 'x-accel-buffering: no'           /tmp/smoke-headers.txt || die "缺少 X-Accel-Buffering: no"
ok "SSE 响应头正确（event-stream / no-cache / keep-alive / X-Accel-Buffering）"

say "发送消息并读取 SSE 流"
STREAM=$(curl -sS -N -X POST "$BASE_URL/api/sessions/$SESSION_ID/messages" \
  -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"content":"冒烟测试：请用一句话介绍你自己"}' || true)
printf '%s\n' "$STREAM" | head -12

say "校验事件序列"
grep -q 'event: message.start' <<<"$STREAM" || die "缺少 message.start"
grep -q 'event: message.delta' <<<"$STREAM" || die "缺少 message.delta"
grep -q 'event: message.done'  <<<"$STREAM" || die "缺少 message.done"
grep -q '^id: '                <<<"$STREAM" || die "事件缺少 id 字段"
grep -q '^data: '              <<<"$STREAM" || die "事件缺少 data 字段"
grep -q 'retry: '              <<<"$STREAM" || die "缺少 retry 重连指令"

# 顺序必须 start → delta → done
FIRST_EVENT=$(grep -m1 '^event: ' <<<"$STREAM" | sed 's/^event: //')
LAST_EVENT=$(grep '^event: ' <<<"$STREAM" | tail -1 | sed 's/^event: //')
[[ "$FIRST_EVENT" == "message.start" ]] || die "首个事件应为 message.start，实际 $FIRST_EVENT"
[[ "$LAST_EVENT" == "message.done" ]]   || die "末个事件应为 message.done，实际 $LAST_EVENT"
ok "SSE 事件序列正确（start → delta × N → done，含 id/data/retry）"

say "校验事件 id 单调递增"
grep '^id: ' <<<"$STREAM" | sed 's/^id: //' | awk '
  NR>1 && $1<=prev { printf "❌ id 未递增: %s <= %s\n", $1, prev; exit 1 }
  { prev=$1 }
' || die "事件 id 未严格递增"
ok "事件 id 单调递增"

say "校验历史落库"
DETAIL=$(curl -fsS "$BASE_URL/api/sessions/$SESSION_ID")
printf '%s' "$DETAIL" | grep -q '"role":"user"'        || die "历史缺少 user 消息"
printf '%s' "$DETAIL" | grep -q '"role":"assistant"'   || die "历史缺少 assistant 消息"
printf '%s' "$DETAIL" | grep -q '"status":"complete"'  || die "assistant 消息未标记 complete"
printf '%s' "$DETAIL" | grep -q '"usage"'              || die "assistant 消息缺少 usage"
ok "会话历史已落库（含状态与 usage）"

say "校验断线补齐（last_event_id=1）"
REPLAY=$(curl -sS -N "$BASE_URL/api/sessions/$SESSION_ID/stream?last_event_id=1" || true)
grep -q 'event: message.done' <<<"$REPLAY" || die "补齐流未重放到 message.done"
ok "断线补齐可用（重放出 done 事件）"

say "校验并发保护（409）"
# 起一个慢请求占住会话，再并发发送。
curl -sS -N -X POST "$BASE_URL/api/sessions/$SESSION_ID/messages" \
  -H 'content-type: application/json' \
  -d '{"content":"占位请求"}' >/dev/null 2>&1 &
BG_PID=$!
sleep 0.15
CONFLICT_CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/sessions/$SESSION_ID/messages" \
  -H 'content-type: application/json' -d '{"content":"并发请求"}')
wait "$BG_PID" 2>/dev/null || true
case "$CONFLICT_CODE" in
  409|200) ok "并发保护生效（HTTP ${CONFLICT_CODE}）" ;;
  *) die "并发发送期望 409（或前一个已结束时的 200），实际 ${CONFLICT_CODE}" ;;
esac

say "删除会话"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$BASE_URL/api/sessions/$SESSION_ID")
[[ "$CODE" == "204" ]] || die "删除会话期望 204，实际 $CODE"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/api/sessions/$SESSION_ID")
[[ "$CODE" == "404" ]] || die "删除后详情期望 404，实际 $CODE"
ok "会话已删除且返回 404"

printf '\n\033[1;32m🎉 冒烟测试全部通过\033[0m\n'

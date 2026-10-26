#!/usr/bin/env bash
# ask_user 跨进程恢复的真实端到端验证。
#
# 与 Go 测试的区别：这里用**真实进程**（不是 httptest），
# 真的 kill -9 再重启，验证「关掉窗口明天再回答」这个场景。
# Go 测试里的「重启」只是重建对象，进程没死；这里进程是真的死了。
#
# 用法：bash scripts/e2e-ask-user.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-18111}"
DATA_DIR="$(mktemp -d)"
BIN="$(mktemp -d)/agent-server"
GW_PORT="${GW_PORT:-18112}"
LOG_DIR="$(mktemp -d)"

cleanup() {
  [ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
  [ -n "${SRV_PID:-}" ] && kill -9 "$SRV_PID" 2>/dev/null
  rm -rf "$DATA_DIR" "$LOG_DIR"
}
trap cleanup EXIT

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[36m▶ %s\033[0m\n' "$*"; }

FAILED=0
check() { # check <描述> <实际> <期望>
  if [ "$2" = "$3" ]; then
    green "  ✓ $1"
  else
    red "  ✗ $1（实际: $2，期望: $3）"
    FAILED=$((FAILED + 1))
  fi
}
contains() { # contains <描述> <文本> <子串>
  case "$2" in
    *"$3"*) green "  ✓ $1" ;;
    *) red "  ✗ $1（未找到 \"$3\"，实际: $2）"; FAILED=$((FAILED + 1)) ;;
  esac
}

step "构建服务二进制"
( cd "$ROOT" && go build -o "$BIN" ./cmd/server ) || { red "构建失败"; exit 1; }
green "  ✓ $BIN"

step "启动假模型网关（首轮 ask_user，之后给最终答案）"
cat > "$LOG_DIR/gw.mjs" <<'EOF'
import http from 'node:http'
const PORT = Number(process.env.GW_PORT)
http.createServer((req, res) => {
  let body = ''
  req.on('data', (c) => (body += c))
  req.on('end', () => {
    res.writeHead(200, { 'Content-Type': 'text/event-stream' })
    const hasTool = body.includes('"role":"tool"')
    if (hasTool) {
      res.write('data: {"choices":[{"delta":{"content":"好，就用 SQLite 建表。"}}]}\n\n')
    } else {
      res.write('data: {"choices":[{"delta":{"content":"先确认一下："}}]}\n\n')
      const args = JSON.stringify({ question: '用哪种数据库？', options: ['SQLite', 'Postgres'] })
      res.write(
        'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e2e_1",' +
        '"type":"function","function":{"name":"ask_user","arguments":' +
        JSON.stringify(args) + '}}]}}]}\n\n',
      )
    }
    res.write('data: [DONE]\n\n')
    res.end()
  })
}).listen(PORT)
EOF
GW_PORT="$GW_PORT" node "$LOG_DIR/gw.mjs" > "$LOG_DIR/gw.log" 2>&1 &
GW_PID=$!
sleep 0.6
if ! kill -0 "$GW_PID" 2>/dev/null; then
  red "网关启动失败"; cat "$LOG_DIR/gw.log"; exit 1
fi
green "  ✓ 网关 :$GW_PORT"

start_server() {
  LLM_PROTOCOL=openai \
  LLM_PROVIDER=ollama \
  LLM_BASE_URL="http://127.0.0.1:$GW_PORT/v1" \
  LLM_API_KEY=k \
  LLM_MODEL=mock \
  DATA_DIR="$DATA_DIR" \
  STORE_DRIVER=sqlite \
  PORT="$PORT" \
  TOOL_WORKSPACE="$DATA_DIR" \
  RUN_TIMEOUT=60s \
  "$BIN" >> "$LOG_DIR/server.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 50); do
    if curl -fsS -m 1 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  red "服务未能就绪"; tail -20 "$LOG_DIR/server.log"; return 1
}

step "第一次启动服务（空库）"
start_server || exit 1
green "  ✓ 服务 :$PORT"

step "创建会话并发起提问"
SID=$(curl -fsS -m 5 -X POST "http://127.0.0.1:$PORT/api/sessions" \
  -H 'Content-Type: application/json' -d '{"title":"e2e"}' |
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).session.id))')
contains "会话已创建" "$SID" "sess_"
echo "    session_id=$SID"

curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID/messages" \
  -H 'Content-Type: application/json' -d '{"content":"帮我建个用户表"}' \
  > "$LOG_DIR/stream1.txt" 2>&1

contains "收到 awaiting.input 事件" "$(cat "$LOG_DIR/stream1.txt")" "event: awaiting.input"
if grep -q "event: message.done" "$LOG_DIR/stream1.txt"; then
  red "  ✗ 挂起时不应有 message.done"; FAILED=$((FAILED + 1))
else
  green "  ✓ 挂起时没有 message.done"
fi

step "直接查数据库，确认 session_id 写进了 checkpoints 表"
DB="$DATA_DIR/agent.db"
DB_SID=$(sqlite3 "$DB" "select session_id from checkpoints where state='suspended';" 2>/dev/null)
check "checkpoints.session_id 与请求一致" "$DB_SID" "$SID"
DB_Q=$(sqlite3 "$DB" "select json_extract(payload,'\$.pendings[0].question') from checkpoints where state='suspended';" 2>/dev/null)
check "问题文本已落库" "$DB_Q" "用哪种数据库？"

DB_N=$(sqlite3 "$DB" "select json_array_length(json_extract(payload,'\$.pendings')) from checkpoints where state='suspended';" 2>/dev/null)
check "待回答项数量" "$DB_N" "1"

step "kill -9 强杀服务（模拟崩溃/关窗口）"
kill -9 "$SRV_PID" 2>/dev/null
wait "$SRV_PID" 2>/dev/null
SRV_PID=""
sleep 0.4
green "  ✓ 进程已强杀"

step "重启服务（同一个数据库）"
start_server || exit 1
green "  ✓ 服务已重启"

step "校验挂起状态在重启后依然存在"
CP_JSON=$(curl -fsS -m 5 "http://127.0.0.1:$PORT/api/sessions/$SID/checkpoint")
contains "checkpoint 报告 suspended" "$CP_JSON" '"suspended":true'
CALL_ID=$(printf '%s' "$CP_JSON" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).call_id||""))')
check "call_id 完整保留" "$CALL_ID" "call_e2e_1"

SESS_STATUS=$(curl -fsS -m 5 "http://127.0.0.1:$PORT/api/sessions/$SID" |
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).session.status))')
check "会话状态未被误清成 idle" "$SESS_STATUS" "suspended"

step "resume：提交回答并继续生成"
curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID/resume" \
  -H 'Content-Type: application/json' \
  -d "{\"call_id\":\"$CALL_ID\",\"answer\":\"用 SQLite\"}" \
  > "$LOG_DIR/stream2.txt" 2>&1

contains "resume 后收到 message.done" "$(cat "$LOG_DIR/stream2.txt")" "event: message.done"
contains "收到了最终回答" "$(cat "$LOG_DIR/stream2.txt")" "SQLite 建表"

step "校验 resume 后状态收敛"
CP_AFTER=$(curl -fsS -m 5 "http://127.0.0.1:$PORT/api/sessions/$SID/checkpoint")
contains "checkpoint 已清理" "$CP_AFTER" '"suspended":false'

TOOL_ROWS=$(sqlite3 "$DB" "select count(*) from messages where session_id='$SID' and role='tool' and tool_call_id='$CALL_ID';" 2>/dev/null)
check "用户回答落成 tool 消息" "$TOOL_ROWS" "1"

TOOL_CONTENT=$(sqlite3 "$DB" "select content from messages where session_id='$SID' and role='tool' and tool_call_id='$CALL_ID';" 2>/dev/null)
check "tool 消息内容即用户答复" "$TOOL_CONTENT" "用 SQLite"

step "校验历史完整（无孤儿 tool_calls）"
ORPHANS=$(sqlite3 "$DB" "
  select count(*) from messages a
  where a.session_id='$SID' and a.role='assistant'
    and a.tool_calls != ''
    and not exists (
      select 1 from messages t
      where t.session_id=a.session_id and t.role='tool'
        and t.tool_call_id = json_extract(a.tool_calls,'\$[0].id')
    );" 2>/dev/null)
check "没有未应答的 tool_calls" "$ORPHANS" "0"

echo
if [ "$FAILED" -eq 0 ]; then
  green "✅ 跨进程恢复验证全部通过"
  exit 0
fi
red "❌ 有 $FAILED 项失败"
echo "--- 服务日志尾部 ---"
tail -30 "$LOG_DIR/server.log"
exit 1

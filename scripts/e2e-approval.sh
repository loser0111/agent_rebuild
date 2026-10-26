#!/usr/bin/env bash
# 人工审批（manual 模式）端到端验证：真实走一遍 HTTP + SSE 全链路，
# 不依赖真实模型 Key —— 用一个假的 OpenAI 兼容网关，脚本化地让"模型"
# 请求调用 write_file，分别验证「批准后真的写了文件」与
# 「拒绝后文件确实没被创建」两条路径。
#
# 写法参考 scripts/e2e-ask-user.sh：同样的假网关套路，
# 只是这次挂起原因是 PendingApproval 而不是 PendingInput。
#
# 用法：bash scripts/e2e-approval.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-18121}"
GW_PORT="${GW_PORT:-18122}"
DATA_DIR="$(mktemp -d)"
WORKSPACE="$(mktemp -d)"
BIN="$(mktemp -d)/agent-server"
LOG_DIR="$(mktemp -d)"

cleanup() {
  [ -n "${GW_PID:-}" ] && kill "$GW_PID" 2>/dev/null
  [ -n "${SRV_PID:-}" ] && kill -9 "$SRV_PID" 2>/dev/null
  rm -rf "$DATA_DIR" "$WORKSPACE" "$LOG_DIR"
}
trap cleanup EXIT

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[36m▶ %s\033[0m\n' "$*"; }

FAILED=0
check() { # check <描述> <实际> <期望>
  if [ "$2" = "$3" ]; then green "  ✓ $1"; else
    red "  ✗ $1（实际: $2，期望: $3）"; FAILED=$((FAILED + 1)); fi
}
contains() { # contains <描述> <文本> <子串>
  case "$2" in
    *"$3"*) green "  ✓ $1" ;;
    *) red "  ✗ $1（未找到 \"$3\"，实际前 300 字: ${2:0:300}）"; FAILED=$((FAILED + 1)) ;;
  esac
}
not_contains() { # not_contains <描述> <文本> <子串>
  case "$2" in
    *"$3"*) red "  ✗ $1（不应出现 \"$3\"）"; FAILED=$((FAILED + 1)) ;;
    *) green "  ✓ $1" ;;
  esac
}

step "构建服务二进制"
( cd "$ROOT" && go build -o "$BIN" ./cmd/server ) || { red "构建失败"; exit 1; }
green "  ✓ $BIN"

step "启动假模型网关（固定请求 write_file；拿到 tool 结果后直接收尾）"
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
      res.write('data: {"choices":[{"delta":{"content":"已处理完毕。"}}]}\n\n')
    } else {
      res.write('data: {"choices":[{"delta":{"content":"好的，我来写文件。"}}]}\n\n')
      const args = JSON.stringify({ path: 'approved.txt', content: 'hello from e2e' })
      res.write(
        'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e2e_wf","' +
        'type":"function","function":{"name":"write_file","arguments":' +
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

step "启动服务（APPROVAL_MODE=manual）"
LLM_PROTOCOL=openai \
LLM_PROVIDER=ollama \
LLM_BASE_URL="http://127.0.0.1:$GW_PORT/v1" \
LLM_API_KEY=k \
LLM_MODEL=mock \
DATA_DIR="$DATA_DIR" \
STORE_DRIVER=sqlite \
PORT="$PORT" \
TOOL_WORKSPACE="$WORKSPACE" \
APPROVAL_MODE=manual \
RUN_TIMEOUT=60s \
"$BIN" > "$LOG_DIR/server.log" 2>&1 &
SRV_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -m 1 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
  sleep 0.2
done
if ! curl -fsS -m 1 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1; then
  red "服务未能就绪"; tail -30 "$LOG_DIR/server.log"; exit 1
fi
green "  ✓ 服务 :$PORT（workspace=$WORKSPACE）"

new_session() {
  curl -fsS -m 5 -X POST "http://127.0.0.1:$PORT/api/sessions" \
    -H 'Content-Type: application/json' -d '{"title":"e2e-approval"}' |
    node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).session.id))'
}

# ============================================================
# 场景一：批准 —— 文件应该真的被写入
# ============================================================
step "场景一：请求写文件并批准"
SID1=$(new_session)
contains "会话已创建" "$SID1" "sess_"

curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID1/messages" \
  -H 'Content-Type: application/json' -d '{"content":"帮我创建 approved.txt"}' \
  > "$LOG_DIR/s1_turn1.txt" 2>&1

contains "收到 tool.approval 事件（而不是直接执行）" "$(cat "$LOG_DIR/s1_turn1.txt")" "event: tool.approval"
if grep -q "event: message.done" "$LOG_DIR/s1_turn1.txt"; then
  red "  ✗ 挂起时不应有 message.done"; FAILED=$((FAILED + 1))
else
  green "  ✓ 挂起时没有 message.done"
fi
[ -f "$WORKSPACE/approved.txt" ] && { red "  ✗ 批准前文件就已存在（审批形同虚设）"; FAILED=$((FAILED + 1)); } \
  || green "  ✓ 批准前文件确实不存在"

CP1=$(curl -fsS -m 5 "http://127.0.0.1:$PORT/api/sessions/$SID1/checkpoint")
contains "checkpoint 报告 suspended" "$CP1" '"suspended":true'
KIND1=$(printf '%s' "$CP1" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).questions[0].kind))')
check "挂起类型是 approval" "$KIND1" "approval"
NAME1=$(printf '%s' "$CP1" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).questions[0].name))')
check "工具名正确" "$NAME1" "write_file"
CALL1=$(printf '%s' "$CP1" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).call_id))')

step "提交批准决定"
curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID1/resume" \
  -H 'Content-Type: application/json' \
  -d "{\"decisions\":{\"$CALL1\":\"approve\"}}" \
  > "$LOG_DIR/s1_turn2.txt" 2>&1

contains "resume 后收到 message.done" "$(cat "$LOG_DIR/s1_turn2.txt")" "event: message.done"
if [ -f "$WORKSPACE/approved.txt" ]; then
  green "  ✓ 批准后文件被真正创建"
  CONTENT=$(cat "$WORKSPACE/approved.txt")
  check "文件内容正确" "$CONTENT" "hello from e2e"
else
  red "  ✗ 批准后文件没有被创建（ExecuteApproved 没真正执行）"; FAILED=$((FAILED + 1))
fi

# ============================================================
# 场景二：拒绝 —— 文件不应被创建
# ============================================================
step "场景二：请求写另一个文件并拒绝"
SID2=$(new_session)
curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID2/messages" \
  -H 'Content-Type: application/json' -d '{"content":"帮我创建 approved.txt"}' \
  > "$LOG_DIR/s2_turn1.txt" 2>&1
contains "收到 tool.approval 事件" "$(cat "$LOG_DIR/s2_turn1.txt")" "event: tool.approval"

CP2=$(curl -fsS -m 5 "http://127.0.0.1:$PORT/api/sessions/$SID2/checkpoint")
CALL2=$(printf '%s' "$CP2" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).call_id))')

# 先删掉场景一留下的文件，确保这次检测的是"这次调用"而不是遗留文件。
rm -f "$WORKSPACE/approved.txt"

step "提交拒绝决定"
curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID2/resume" \
  -H 'Content-Type: application/json' \
  -d "{\"decisions\":{\"$CALL2\":\"deny\"},\"reason\":\"e2e 测试拒绝\"}" \
  > "$LOG_DIR/s2_turn2.txt" 2>&1

contains "resume 后收到 message.done" "$(cat "$LOG_DIR/s2_turn2.txt")" "event: message.done"
if [ -f "$WORKSPACE/approved.txt" ]; then
  red "  ✗ 拒绝后文件竟然被创建了（审批被绕过）"; FAILED=$((FAILED + 1))
else
  green "  ✓ 拒绝后文件确实没有被创建"
fi

DB="$DATA_DIR/agent.db"
TOOL_CONTENT=$(sqlite3 "$DB" "select content from messages where session_id='$SID2' and role='tool' and tool_call_id='$CALL2';" 2>/dev/null)
contains "tool 结果说明了拒绝理由" "$TOOL_CONTENT" "e2e 测试拒绝"

echo
if [ "$FAILED" -eq 0 ]; then
  green "🎉 人工审批（manual）全部通过"
  exit 0
fi
red "❌ 有 $FAILED 项失败"
echo "--- 服务日志尾部 ---"
tail -30 "$LOG_DIR/server.log"
exit 1

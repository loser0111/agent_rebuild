#!/usr/bin/env bash
# 「一次 ask_user 问多个问题」的真实进程验证。
#
# 与 e2e-ask-user.sh 的区别：那个验证单个问题的跨进程恢复，
# 这里验证**一次调用装多个问题**——这是「减少往返次数」的核心。
#
# 关键断言：
#   1. 一个 call_id 下挂起多个问题（qid 各不相同）
#   2. 批量提交后只写**一条** tool 结果（多条会让 OpenAI 400）
#   3. 那条结果里含全部问答对
#
# 用法：bash scripts/e2e-multi-question.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${PORT:-18121}"
GW_PORT="${GW_PORT:-18122}"
DATA_DIR="$(mktemp -d)"
LOG_DIR="$(mktemp -d)"
BIN="$(mktemp -d)/agent-server"

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
check() {
  if [ "$2" = "$3" ]; then green "  ✓ $1"; else
    red "  ✗ $1（实际: $2，期望: $3）"; FAILED=$((FAILED + 1)); fi
}
contains() {
  case "$2" in
    *"$3"*) green "  ✓ $1" ;;
    *) red "  ✗ $1（未找到 \"$3\"）"; FAILED=$((FAILED + 1)) ;;
  esac
}

step "构建服务二进制"
( cd "$ROOT" && go build -o "$BIN" ./cmd/server ) || { red "构建失败"; exit 1; }
green "  ✓ $BIN"

step "启动假模型网关（一次调用问 3 个问题）"
cat > "$LOG_DIR/gw.mjs" <<'EOF'
import http from 'node:http'
const PORT = Number(process.env.GW_PORT)
http.createServer((req, res) => {
  let body = ''
  req.on('data', (c) => (body += c))
  req.on('end', () => {
    res.writeHead(200, { 'Content-Type': 'text/event-stream' })
    // 历史里出现 tool 结果 → 说明用户已回答。
    if (body.includes('"role":"tool"')) {
      res.write('data: {"choices":[{"delta":{"content":"好，全部照办。"}}]}\n\n')
      res.write('data: [DONE]\n\n')
      res.end()
      return
    }
    // 一次调用，三个问题。
    const args = JSON.stringify({
      questions: [
        { id: 'db', question: '用哪种数据库？', options: ['SQLite', 'Postgres'] },
        { id: 'scope', question: '改哪些文件？' },
        { question: '要补测试吗？', options: ['要', '不要'] },
      ],
    })
    res.write('data: {"choices":[{"delta":{"content":"先确认三件事："}}]}\n\n')
    res.write(
      'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_multi",' +
      '"type":"function","function":{"name":"ask_user","arguments":' +
      JSON.stringify(args) + '}}]}}]}\n\n',
    )
    res.write('data: [DONE]\n\n')
    res.end()
  })
}).listen(PORT)
EOF
GW_PORT="$GW_PORT" node "$LOG_DIR/gw.mjs" > "$LOG_DIR/gw.log" 2>&1 &
GW_PID=$!
sleep 0.6
kill -0 "$GW_PID" 2>/dev/null || { red "网关启动失败"; cat "$LOG_DIR/gw.log"; exit 1; }
green "  ✓ 网关 :$GW_PORT"

step "启动服务"
LLM_PROTOCOL=openai LLM_PROVIDER=ollama \
LLM_BASE_URL="http://127.0.0.1:$GW_PORT/v1" LLM_API_KEY=k LLM_MODEL=mock \
DATA_DIR="$DATA_DIR" STORE_DRIVER=sqlite PORT="$PORT" \
TOOL_WORKSPACE="$DATA_DIR" RUN_TIMEOUT=60s \
"$BIN" >> "$LOG_DIR/server.log" 2>&1 &
SRV_PID=$!
for _ in $(seq 1 50); do
  curl -fsS -m 1 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS -m 2 "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 || {
  red "服务未就绪"; tail -20 "$LOG_DIR/server.log"; exit 1; }
green "  ✓ 服务 :$PORT"

step "创建会话并触发多问题提问"
SID=$(curl -fsS -m 5 -X POST "http://127.0.0.1:$PORT/api/sessions" \
  -H 'Content-Type: application/json' -d '{"title":"multi-e2e"}' |
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).session.id))')
contains "会话已创建" "$SID" "sess_"

curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID/messages" \
  -H 'Content-Type: application/json' -d '{"content":"开始"}' > "$LOG_DIR/s1.txt" 2>&1

EVENT_COUNT=$(grep -c "event: awaiting.input" "$LOG_DIR/s1.txt" || true)
check "收到 3 个 awaiting.input 事件" "$EVENT_COUNT" "3"

CALL_UNIQ=$(grep -A1 "event: awaiting.input" "$LOG_DIR/s1.txt" |
  grep "^data:" | sed 's/^data: //' |
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{
    const ids=s.trim().split("\n").filter(Boolean).map(l=>JSON.parse(l));
    console.log(new Set(ids.map(x=>x.call_id)).size);
  })')
check "3 个问题共享 1 个 call_id" "$CALL_UNIQ" "1"

QID_UNIQ=$(grep -A1 "event: awaiting.input" "$LOG_DIR/s1.txt" |
  grep "^data:" | sed 's/^data: //' |
  node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{
    const ids=s.trim().split("\n").filter(Boolean).map(l=>JSON.parse(l));
    console.log(new Set(ids.map(x=>x.qid)).size);
  })')
check "3 个问题的 qid 互不相同" "$QID_UNIQ" "3"

step "校验 checkpoint 里存了 3 个待回答项"
DB="$DATA_DIR/agent.db"
DB_N=$(sqlite3 "$DB" "select json_array_length(json_extract(payload,'\$.pendings')) from checkpoints where state='suspended';" 2>/dev/null)
check "checkpoint 有 3 个 pendings" "$DB_N" "3"

DB_Q2=$(sqlite3 "$DB" "select json_extract(payload,'\$.pendings[1].question') from checkpoints where state='suspended';" 2>/dev/null)
check "第二个问题内容正确" "$DB_Q2" "改哪些文件？"

step "批量提交 3 个答案"
curl -fsS -m 20 -N -X POST "http://127.0.0.1:$PORT/api/sessions/$SID/resume" \
  -H 'Content-Type: application/json' \
  -d '{"answers":{"call_multi::db":"SQLite","call_multi::scope":"src/","call_multi::q3":"要"}}' \
  > "$LOG_DIR/s2.txt" 2>&1

contains "resume 后收到最终回答" "$(cat "$LOG_DIR/s2.txt")" "全部照办"

step "校验只写了一条 tool 结果"
TOOL_N=$(sqlite3 "$DB" "select count(*) from messages where session_id='$SID' and role='tool';" 2>/dev/null)
check "tool 结果只有 1 条（多条会让 OpenAI 400）" "$TOOL_N" "1"

TOOL_CID=$(sqlite3 "$DB" "select tool_call_id from messages where session_id='$SID' and role='tool';" 2>/dev/null)
check "tool 结果的 call_id 正确" "$TOOL_CID" "call_multi"

TOOL_CONTENT=$(sqlite3 "$DB" "select content from messages where session_id='$SID' and role='tool';" 2>/dev/null)
contains "结果含第 1 问答对" "$TOOL_CONTENT" "用哪种数据库？"
contains "结果含第 2 问答对" "$TOOL_CONTENT" "改哪些文件？"
contains "结果含第 3 问答对" "$TOOL_CONTENT" "要补测试吗？"

step "校验没有孤儿 tool_calls"
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
  green "✅ 多问题单调用验证全部通过"
  exit 0
fi
red "❌ 有 $FAILED 项失败"
echo "--- 服务日志 ---"
tail -30 "$LOG_DIR/server.log"
exit 1

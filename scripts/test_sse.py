#!/usr/bin/env python3
import requests
import json
import time
from typing import Generator

BASE_URL = "http://localhost:8080/api"

def create_session(title="测试会话"):
    """创建新会话"""
    resp = requests.post(f"{BASE_URL}/sessions", json={"title": title})
    resp.raise_for_status()
    return resp.json()["session"]["id"]

def send_message_stream(session_id: str, content: str) -> Generator[dict, None, None]:
    """发送消息并流式接收SSE事件"""
    resp = requests.post(
        f"{BASE_URL}/sessions/{session_id}/messages",
        json={"content": content, "stream": True},
        stream=True,
        timeout=180
    )
    resp.raise_for_status()
    
    current_event = {}
    for line in resp.iter_lines():
        if not line:
            # 空行：事件结束
            if current_event:
                yield current_event
                current_event = {}
            continue
        
        line_str = line.decode('utf-8') if isinstance(line, bytes) else line
        
        if line_str.startswith('id: '):
            current_event['id'] = int(line_str[4:])
        elif line_str.startswith('event: '):
            current_event['event'] = line_str[7:]
        elif line_str.startswith('data: '):
            try:
                current_event['data'] = json.loads(line_str[6:])
            except json.JSONDecodeError:
                current_event['data'] = line_str[6:]
        elif line_str.startswith('retry: '):
            current_event['retry'] = int(line_str[7:])
        elif line_str.startswith(': '):
            # 注释行，忽略
            pass

def test_multiple_tools_with_text():
    """测试多个工具调用后的文本是否完整"""
    
    session_id = create_session("多工具测试")
    print(f"✓ 创建会话: {session_id}")
    print("\n" + "="*80)
    print("发送消息：多个工具调用 + 文本输出")
    print("="*80 + "\n")
    
    # 发送包含多个工具调用的消息
    content = """请按顺序执行：
1. 列出 internal/api 目录下的所有文件
2. 搜索所有包含 'message' 的 Go 文件
3. 然后给我总结一下你找到了什么文件，并提供一个简短的分析
"""
    
    print(f"📤 发送消息:\n{content}\n")
    
    # 记录所有收到的事件
    events = []
    message_id = None
    accumulated_content = ""
    delta_count = 0
    tool_calls = {}
    tool_results = {}
    
    print("📥 接收事件流:\n")
    
    for event in send_message_stream(session_id, content):
        event_name = event.get('event', 'unknown')
        event_id = event.get('id', '?')
        
        events.append(event)
        
        if event_name == 'message.start':
            message_id = event['data'].get('message_id')
            print(f"  [ID:{event_id}] message.start: {message_id}")
        
        elif event_name == 'message.delta':
            delta = event['data'].get('delta', '')
            accumulated_content += delta
            delta_count += 1
            if len(delta) < 50:
                print(f"  [ID:{event_id}] message.delta: {repr(delta)}")
            else:
                print(f"  [ID:{event_id}] message.delta: {repr(delta[:50])}... (+{len(delta)-50} chars)")
        
        elif event_name == 'tool.call':
            call_id = event['data'].get('call_id')
            tool_name = event['data'].get('name')
            tool_calls[call_id] = tool_name
            print(f"  [ID:{event_id}] tool.call: {tool_name} (id:{call_id})")
        
        elif event_name == 'tool.result':
            call_id = event['data'].get('call_id')
            is_error = event['data'].get('is_error', False)
            tool_results[call_id] = is_error
            status = "❌ ERROR" if is_error else "✓ SUCCESS"
            output = event['data'].get('output', '')
            if len(output) > 60:
                output = output[:60] + f"... (+{len(output)-60} chars)"
            print(f"  [ID:{event_id}] tool.result: {status} - {repr(output)}")
        
        elif event_name == 'message.done':
            final_content = event['data'].get('content', '')
            usage = event['data'].get('usage', {})
            print(f"  [ID:{event_id}] message.done: {len(final_content)} chars")
            print(f"             tokens - prompt:{usage.get('prompt_tokens')}, completion:{usage.get('completion_tokens')}")
        
        elif event_name == 'buffer.overflow':
            print(f"  ⚠️  [ID:{event_id}] buffer.overflow: 缓冲溢出警告!")
        
        elif event_name == 'resync':
            print(f"  🔄 [ID:{event_id}] resync: 需要同步完整消息")
        
        elif event_name == 'error':
            error_msg = event['data'].get('error', {}).get('message', '未知错误')
            print(f"  ❌ [ID:{event_id}] error: {error_msg}")
        
        elif event_name != 'ping':
            print(f"  [ID:{event_id}] {event_name}: {event.get('data', '')}")
    
    print("\n" + "="*80)
    print("📊 统计信息")
    print("="*80)
    print(f"✓ 总事件数: {len(events)}")
    print(f"✓ Delta 事件数: {delta_count}")
    print(f"✓ 累积文本长度: {len(accumulated_content)}")
    print(f"✓ 工具调用数: {len(tool_calls)}")
    print(f"  └─ 工具列表: {list(tool_calls.values())}")
    print(f"✓ 工具结果数: {len(tool_results)}")
    
    # 获取最终消息验证完整性
    print("\n" + "="*80)
    print("🔍 验证消息完整性")
    print("="*80)
    
    # 从消息完成事件获取最终内容
    final_msg = None
    for event in events:
        if event.get('event') == 'message.done':
            final_msg = event['data'].get('content', '')
            break
    
    if final_msg:
        print(f"✓ 最终消息长度: {len(final_msg)} 字符")
        print(f"\n📝 最终消息内容（前800字）:\n")
        print(final_msg[:800])
        if len(final_msg) > 800:
            print(f"\n... (还有 {len(final_msg) - 800} 字符)\n")
        
        # 检查是否包含总结性内容
        if "总结" in final_msg or "分析" in final_msg or "找到" in final_msg or "文件" in final_msg:
            print("\n✅ 发现总结/分析内容 - 文本输出完整!")
        else:
            print("\n⚠️  可能缺少总结/分析内容 - 需要检查是否丢失")
    
    return len(final_msg) if final_msg else 0

if __name__ == "__main__":
    try:
        content_len = test_multiple_tools_with_text()
        print("\n" + "="*80)
        print(f"✅ 测试完成! 最终内容长度: {content_len} 字符")
        print("="*80)
    except Exception as e:
        print(f"\n❌ 测试失败: {e}")
        import traceback
        traceback.print_exc()

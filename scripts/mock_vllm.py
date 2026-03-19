import json
import time
from flask import Flask, Response, request

app = Flask(__name__)

@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    data = request.json
    stream = data.get('stream', False)
    model = data.get('model', 'mock-qwen')

    if not stream:
        # 非流式响应 (简单返回)
        return json.dumps({
            "id": "chatcmpl-123",
            "object": "chat.completion",
            "choices": [{"message": {"content": "Hello! I am a mock AI."}}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12}
        })

    # 流式响应 (SSE)
    def generate():
        content = "这是一个模拟的流式响应，用于测试 AI 网关的 SSE 转发和 Token 计费逻辑。"
        tokens = content.split(" ") # 简单模拟 token
        
        for i, token in enumerate(tokens):
            chunk = {
                "id": "chatcmpl-123",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {"content": token + " "}, "finish_reason": None}]
            }
            yield f"data: {json.dumps(chunk)}\n\n"
            time.sleep(0.1) # 模拟推理延迟

        # 最后一行返回 usage
        final_chunk = {
            "id": "chatcmpl-123",
            "object": "chat.completion.chunk",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            "usage": {
                "prompt_tokens": 10,
                "completion_tokens": len(tokens),
                "total_tokens": 10 + len(tokens)
            }
        }
        yield f"data: {json.dumps(final_chunk)}\n\n"
        yield "data: [DONE]\n\n"

    return Response(generate(), mimetype='text/event-stream')

if __name__ == '__main__':
    print("🚀 Mock vLLM Server running on http://localhost:8000")
    app.run(port=8000)

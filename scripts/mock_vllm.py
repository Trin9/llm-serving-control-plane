import json
import time
from flask import Flask, Response, request

app = Flask(__name__)

@app.route('/health', methods=['GET'])
def health():
    return "OK", 200

@app.route('/v1/chat/completions', methods=['POST'])
def chat_completions():
    data = request.json
    stream = data.get('stream', False)
    model = data.get('model', 'mock-qwen')

    if not stream:
        # Non-streaming response (simple return)
        return json.dumps({
            "id": "chatcmpl-123",
            "object": "chat.completion",
            "choices": [{"message": {"content": "Hello! I am a mock AI."}}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12}
        })

    # Streaming response (SSE)
    def generate():
        content = "This is a mocked streaming response for testing the AI gateway's SSE forwarding and token billing logic."
        tokens = content.split(" ") # simple token simulation
        
        for i, token in enumerate(tokens):
            chunk = {
                "id": "chatcmpl-123",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {"content": token + " "}, "finish_reason": None}]
            }
            yield f"data: {json.dumps(chunk)}\n\n"
            time.sleep(0.1) # simulate inference latency

        # Return usage on the final line
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
    print("🚀 Mock vLLM Server running on http://0.0.0.0:8000")
    # Must bind to 0.0.0.0, otherwise other Pods in K8s cannot reach it
    app.run(host='0.0.0.0', port=8000)

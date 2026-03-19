# Stage 1: The Builder Stage
FROM golang:1.24-alpine AS builder

# 设置工作目录
WORKDIR /app

# 配置 Go 代理（解决国内网络模块下载失败的问题）
ENV GOPROXY=https://goproxy.cn,direct

# 复制 go.mod 和 go.sum，并下载依赖，这是为了利用 Docker 缓存
COPY go.mod go.sum ./
RUN go mod download

# 复制整个项目代码
COPY . .

# 编译 Go 应用
# CGO_ENABLED=0：禁用 CGO，确保生成的是静态链接的二进制文件
# -a：强制重新构建依赖（确保干净构建）
# -ldflags "-s -w": 移除符号表和调试信息，进一步减小二进制文件体积
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-s -w" -o gate-service ./app/cmd

# Stage 2: The Final (Runtime) Stage
# 使用与 Operator 相同的 distroless 镜像，提供最高级别的安全性（无 Shell）和最小的体积
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/gcr.io/distroless/static:nonroot

# 设置工作目录为根目录
WORKDIR /

# 从 builder 阶段复制编译好的二进制文件
COPY --from=builder /app/gate-service .

# 以 nonroot 用户运行 (UID 65532)
USER 65532:65532

# 暴露服务端口 (根据你的 Go 应用实际监听的端口调整)
EXPOSE 8080

# 定义容器启动时执行的命令
ENTRYPOINT ["/gate-service"]

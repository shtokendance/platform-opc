# ==============================================================================
# Platform-OPC 统一构建 Dockerfile
# 制品统一命名：platform-opc
# 镜像统一命名：platform-opc:<tag>
# 容器统一命名：platform-opc
# ==============================================================================

# -------------------- Stage 1: 依赖与代码编译 --------------------
FROM golang:1.24-bookworm AS builder

WORKDIR /build

# 先复制依赖定义，充分利用 Docker 缓存层
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 编译生成静态二进制，产物统一命名为 platform-opc
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /build/platform-opc .

# -------------------- Stage 2: 最小运行时环境 --------------------
FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 拷贝二进制
COPY --from=builder /build/platform-opc /app/platform-opc
COPY config.example.yaml /app/config.yaml

ENV TZ=Asia/Shanghai
EXPOSE 3002

ENTRYPOINT ["/app/platform-opc", "/app/config.yaml"]

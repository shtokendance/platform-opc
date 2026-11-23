# ==============================================================================
# Platform-OPC 统一构建 Dockerfile
# 制品统一命名：platform-opc
# 镜像统一命名：platform-opc:<tag>
# 容器统一命名：platform-opc
# ==============================================================================

ARG OPC_POSTGRES_TOOLS_IMAGE=postgres:16-bookworm

# -------------------- Stage 1: 依赖与代码编译 --------------------
FROM golang:1.26.5-bookworm AS builder

WORKDIR /build

ARG GOPROXY_ARG
ENV GOPROXY=${GOPROXY_ARG:-https://goproxy.cn,direct}

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

# 应用数据库使用 PostgreSQL 16。备份与恢复工具随运行镜像一起发布。
FROM ${OPC_POSTGRES_TOOLS_IMAGE} AS postgres-tools
RUN mkdir -p /opc-postgres/bin /opc-postgres/lib \
    && cp "/usr/lib/postgresql/${PG_MAJOR}/bin/pg_dump" "/usr/lib/postgresql/${PG_MAJOR}/bin/pg_restore" /opc-postgres/bin/ \
    && cp -L /usr/lib/*-linux-gnu/libpq.so.5 /opc-postgres/lib/libpq.so.5

# -------------------- Stage 2: 最小运行时环境 --------------------
FROM debian:12-slim

ARG APK_MIRROR_ARG
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i -e "s@deb.debian.org@${APK_MIRROR_ARG}@g" -e "s@security.debian.org@${APK_MIRROR_ARG}@g" \
            /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true; \
    fi && \
    apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    curl \
    libgssapi-krb5-2 libldap-2.5-0 liblz4-1 libssl3 libzstd1 zlib1g \
    && rm -rf /var/lib/apt/lists/*

COPY --from=postgres-tools /opc-postgres/bin/ /usr/local/bin/
COPY --from=postgres-tools /opc-postgres/lib/ /usr/local/lib/
RUN ldconfig && pg_dump --version && pg_restore --version

WORKDIR /app

# 拷贝二进制
COPY --from=builder /build/platform-opc /app/platform-opc
COPY config.example.yaml /app/config.yaml

ENV TZ=Asia/Shanghai
EXPOSE 3002

ENTRYPOINT ["/app/platform-opc", "/app/config.yaml"]

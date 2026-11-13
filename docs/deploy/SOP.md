# Platform-OPC 生产环境部署标准作业程序 (SOP)

本文档是 **Platform-OPC**（网关核心服务）上线生产环境的标准化全流程操作指南。

---

## 目录
- [一、全局命名规范](#一全局命名规范)
- [二、前置基础设施准备](#二前置基础设施准备)
- [三、配置文件与环境变量准备](#三配置文件与环境变量准备)
- [四、构建制品与 Docker 镜像](#四构建制品与-docker-镜像)
- [五、服务部署与编排启动](#五服务部署与编排启动)
- [六、健康检查与验收验证](#六健康检查与验收验证)
- [七、管理员令牌签发与后台登录](#七管理员令牌签发与后台登录)
- [八、生产 Nginx 反向代理与 SSL 配置](#八生产-nginx-反向代理与-ssl-配置)
- [九、日常运维、备份与平滑回滚](#九日常运维备份与平滑回滚)

---

## 一、全局命名规范

为避免测试环境历史命名（如 `weknora-gateway`、`tokendance-gateway`）混杂，在正式生产环境中一律遵循统一的 **`platform-opc`** 命名规范：

| 交付载体 | 统一规范名称 | 示例 / 说明 |
| :--- | :--- | :--- |
| **编译二进制产物 (Binary)** | `platform-opc` | 静态编译的单体 Linux 二进制可执行文件 |
| **Docker 镜像名 (Image)** | `platform-opc:<tag>` | 例如 `platform-opc:v2.0.0` 或 `platform-opc:latest` |
| **Docker 容器名 (Container)** | `platform-opc` | 生产单机或编排中的容器名称 |
| **Compose 服务名 (Service)** | `platform-opc` | `docker-compose.yaml` 中的服务节点键名 |
| **工作目录 (Working Dir)** | `/data/platform-opc` | 宿主机推荐部署路径 |
| **配置与环境变量文件名** | `config.yaml` / `platform-opc.env` | 基础拓扑与敏感凭据 |

---

## 二、前置基础设施准备

在正式部署 `platform-opc` 之前，请确认以下前置服务已就绪：

### 1. 主机与系统环境
- **操作系统**：Linux x86_64（Ubuntu 22.04+ / Debian 12+ / RHEL 8+）
- **推荐硬件**：2 核 CPU / 4GB 内存以上，系统盘保留不少于 50GB 可用空间
- **依赖软件**：Docker 24.0+，Docker Compose v2.20+
- **网络互通**：创建生产 Docker 独立网桥：
  ```bash
  docker network create opc-network || true
  ```

### 2. 关系型数据库 (PostgreSQL 14+)
在生产 PostgreSQL 实例中提前建好以下 3 个数据库并分配专用账号权限：
```sql
-- 1. WeKnora 主业务库 (若同机部署)
CREATE DATABASE weknora;

-- 2. Platform 状态数据库 (存储审计日志、组织同步流水、用户映射账本)
CREATE DATABASE weknora_gateway_state;

-- 3. OPC 应用部署专属治理库 (存储发布应用元数据、Dockerfile 模板、存储卷快照)
CREATE DATABASE "deploy-opc";

-- 4. 创建专用数据库管理用户并授权
CREATE USER opc_platform WITH ENCRYPTED PASSWORD 'your_strong_password_here';
GRANT ALL PRIVILEGES ON DATABASE weknora TO opc_platform;
GRANT ALL PRIVILEGES ON DATABASE weknora_gateway_state TO opc_platform;
GRANT ALL PRIVILEGES ON DATABASE "deploy-opc" TO opc_platform;
```

### 3. 对象存储 (MinIO / S3 兼容存储)
- 创建用于存放应用构建产物与技能包的存储桶：`opc-apps`
- 创建用于存放知识库文档归档的存储桶：`weknora`
- 准备好具备读写该桶权限的 `AccessKey` 与 `SecretKey`。

### 4. 企业微信管理后台配置
在[企业微信管理后台](https://work.weixin.qq.com/)确认以下信息已就绪：
- **企业 ID (`corp_id`)**：`ww` 开头的企业唯一标识
- **自建应用 AgentID 与 Secret**：用于员工单点登录与扫码认证
- **通讯录同步 Secret**：具备读取组织架构与员工信息的权限
- **可信域名配置**：将对外访问域名（如 `opc.yourcompany.com`）配置至应用「网页授权及 JS-SDK」与「企业微信授权登录」可信域名列表中。

---

## 三、配置文件与环境变量准备

在生产主机创建工作目录，例如 `/data/platform-opc`：
```bash
mkdir -p /data/platform-opc
cd /data/platform-opc
```

### 1. 批量生成生产随机安全密钥
执行以下 Linux 命令生成符合规范的生产随机安全秘钥：
```bash
# 生成 32 字节 Hex AES 秘钥 (严格要求 64 字符十六进制，对应 32 字节二进制)
openssl rand -hex 32

# 生成 管理后台秘钥、平台认证秘钥、知识库桥秘钥 (3个独立秘钥)
openssl rand -hex 32
openssl rand -hex 32
openssl rand -hex 32
```

### 2. 配置环境变量文件：`platform-opc.env`
复制 `docs/deploy/platform-opc.env.example` 到工作目录下并命名为 `platform-opc.env`，填入真实凭据：
```bash
cp docs/deploy/platform-opc.env.example /data/platform-opc/platform-opc.env
chmod 600 /data/platform-opc/platform-opc.env
```
主要修改内容包括：
- `WECOM_SECRET` / `WECOM_CONTACT_SECRET`：企微应用真实密钥
- `PLATFORM_ADMIN_KEY`：生成的管理后台 JWT 签名密钥
- `PLATFORM_AUTH_KEY`：生成的平台用户认证密钥
- `PLATFORM_BRIDGE_KEY` / `WEKNORA_JWT_SECRET`：知识库桥接秘钥（与 WeKnora app 保持一致）
- `SYSTEM_AES_KEY`：生成的 64 位十六进制 AES 密钥
- `DB_PASSWORD` / `STATE_DB_PASSWORD`：PostgreSQL 连接密码
- `EXTERNAL_BASE_URL`：生产对外真实访问地址（如 `https://opc.yourcompany.com`）
- `MINIO_*` / `MILVUS_*`：生产对象存储与向量检索连接信息

### 3. 配置基础拓扑文件：`config.yaml`
复制 `docs/deploy/config.example.yaml` 到工作目录下并命名为 `config.yaml`：
```bash
cp docs/deploy/config.example.yaml /data/platform-opc/config.yaml
```
主要修改内容包括：
- `server.external_base_url`：填写生产域名
- `wecom.corp_id`：企业微信实际 CorpID
- `database.host` / `statedb.host`：PostgreSQL 主机名/IP
- `admin.allowed_origins`：填写生产域名（如 `https://opc.yourcompany.com`），用于防 CSRF 攻击
- `app_deploy.caprover_url` / `caprover_password`：应用容器底层编排引擎地址与口令
- `app_deploy.online_resources`：配置可供企业应用一键绑定的生产数据库/Redis/OSS 资源模板

---

## 四、构建制品与 Docker 镜像

### 推荐方案 A：纯 Docker 镜像多阶段构建（CI/CD 或生产机器执行）
使用 `docs/deploy/Dockerfile` 直接从源码编译并构建极简镜像，无需在宿主机安装 Go 环境：

```bash
# 在 platform-opc 源码根目录下执行
docker build -t platform-opc:v2.0.0 -f docs/deploy/Dockerfile .
docker tag platform-opc:v2.0.0 platform-opc:latest

# 如有私有镜像仓库，推送至 Harbor
# docker tag platform-opc:v2.0.0 registry.yourcorp.com/opc/platform-opc:v2.0.0
# docker push registry.yourcorp.com/opc/platform-opc:v2.0.0
```

### 备用方案 B：本地跨平台编译静态二进制
在开发机或构建流水线直接静态编译单体二进制并上传至服务器：
```bash
# 静态交叉编译 Linux amd64 产物，产物命名为 platform-opc
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o platform-opc .

# 上传至生产服务器
scp platform-opc user@your-prod-server:/data/platform-opc/platform-opc
```

---

## 五、服务部署与编排启动

将 `docs/deploy/docker-compose.yaml` 复制到生产服务器 `/data/platform-opc/docker-compose.yaml`：

```yaml
services:
  platform-opc:
    image: platform-opc:latest
    container_name: platform-opc
    restart: always
    ports:
      - "3002:3002"
    environment:
      - TZ=Asia/Shanghai
    env_file:
      - ./platform-opc.env
    volumes:
      - ./config.yaml:/app/config.yaml:ro
      - /etc/ssl/certs:/etc/ssl/certs:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - opc-network

networks:
  opc-network:
    external: true
    name: opc-network
```

> **注意说明**：
> 1. `/etc/ssl/certs:/etc/ssl/certs:ro`：将宿主机 CA 根证书挂入容器，保障容器与企业微信、外部安全 API 通信不报证书信任错误。
> 2. `/var/run/docker.sock:/var/run/docker.sock:ro`：挂载 Docker 套接字，用于应用发布治理中心探测容器运行指标、采集存储卷大小以及安全销毁持久卷。

启动服务：
```bash
cd /data/platform-opc
docker compose up -d
```

---

## 六、健康检查与验收验证

### 1. 检查容器状态
```bash
docker ps --filter name=platform-opc
```
输出状态应为 `Up` 且端口为 `0.0.0.0:3002->3002/tcp`。

### 2. 检查启动日志与自动迁移
```bash
docker logs platform-opc --tail 50
```
正常启动日志特征：
- `[Main] Initializing Storage...`：主库连接成功；
- `[Main] Connecting platform state database...`：平台状态库连接成功；
- `[storage] running migration 000001_...` $\sim$ `000017_...`：**17 个 SQL 版本化迁移按序幂等执行完毕**；
- `[Main] Connecting deploy-opc database (deploy-opc)...`：应用部署专属库自动建表与资源导入完成；
- `[Main] TokenDance Gateway 服务正在监听端口 :3002 ...`：HTTP 网关成功就绪。

### 3. 本地端点连通性测试
```bash
# 测试公共健康状态/登录页重定向
curl -I http://127.0.0.1:3002/admin/login
# 预期返回 HTTP 200 OK
```

---

## 七、管理员令牌签发与后台登录

OPC 管理控制台（`/admin/deploy`）采用无会话状态的 HS256 JWT 安全令牌认证体系。首次部署后，需要通过管理秘钥签发一枚管理员令牌（Token）以登录系统。

### 1. 使用自动化脚本签发 30 天有效令牌
在服务器上执行 `docs/deploy/scripts/generate-admin-token.py`：
```bash
# 方式一：直接读取当前环境变量中的 PLATFORM_ADMIN_KEY
python3 docs/deploy/scripts/generate-admin-token.py

# 方式二：手动传入秘钥与指定有效天数（例如 60 天）
python3 docs/deploy/scripts/generate-admin-token.py "你的PLATFORM_ADMIN_KEY" 60
```

### 2. 登录管理后台
1. 打开浏览器访问：`https://opc.yourcompany.com/admin/login`；
2. 在「管理员访问令牌」输入框中粘贴上述脚本生成的 Token；
3. 点击「登录」，系统将自动完成会话绑定并跳转至 **Deploy-OPC 平台资源与部署治理中心**（`/admin/deploy`）；
4. 核对页面上的应用卡片、基础资源列表（PostgreSQL/Redis/OSS）及 Dockerfile 模板库是否正常显示。

---

## 八、生产 Nginx 反向代理与 SSL 配置

生产环境必须配置反向代理并启用 HTTPS 协议。

推荐的 Nginx 虚拟主机配置模板：
```nginx
server {
    listen 80;
    server_name opc.yourcompany.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name opc.yourcompany.com;

    ssl_certificate     /etc/nginx/ssl/opc.yourcompany.com.crt;
    ssl_certificate_key /etc/nginx/ssl/opc.yourcompany.com.key;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;

    # 允许上传应用制品包与技能包 (最大建议 500M)
    client_max_body_size 500M;

    location / {
        proxy_pass http://127.0.0.1:3002;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # WebSocket 支持 (用于构建日志、实时容器日志的流式传输)
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";

        # 针对长耗时应用部署与下载的超时设置
        proxy_connect_timeout 60s;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

---

## 九、日常运维、备份与平滑回滚

### 1. 日常查看与导出日志
```bash
# 查看实时日志
docker logs -f --tail 100 platform-opc

# 导出最近 24 小时日志
docker logs --since 24h platform-opc > /data/platform-opc/logs_$(date +%Y%m%d).log
```

### 2. 数据库日常备份
建议将平台核心状态库纳入定时备份 Cron 计划：
```bash
# 备份平台状态库
docker exec -t postgres-container pg_dump -U opc_platform weknora_gateway_state > /data/backup/state_$(date +%Y%m%d).sql

# 备份应用部署中心库
docker exec -t postgres-container pg_dump -U opc_platform deploy-opc > /data/backup/deploy_$(date +%Y%m%d).sql
```

### 3. 版本平滑升级与回滚操作

#### 版本升级：
```bash
cd /data/platform-opc
# 1. 备份当前配置
cp platform-opc.env platform-opc.env.bak-$(date +%Y%m%d)
cp config.yaml config.yaml.bak-$(date +%Y%m%d)

# 2. 拉取/导入新镜像
docker pull registry.yourcorp.com/opc/platform-opc:v2.1.0
docker tag registry.yourcorp.com/opc/platform-opc:v2.1.0 platform-opc:latest

# 3. 重新创建容器
docker compose up -d
```

#### 紧急回滚：
若新版本出现不可预知缺陷，执行极速回滚：
```bash
# 重新切回上一版本的镜像 tag
docker tag registry.yourcorp.com/opc/platform-opc:v2.0.0 platform-opc:latest
docker compose up -d --force-recreate
```
由于 SQL 迁移具备单调递增和向下兼容设计，回滚镜像后系统可平稳接管业务。

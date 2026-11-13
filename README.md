# WeKnora TokenDance Gateway (`weknora-tokendance-gateway`)

企业微信组织架构同步与 OAuth2 免密认证网关，专为 WeKnora 知识库系统打造，实现：
1. **部门作为空间（Tenant）**：全自动同步企微部门树到 WeKnora 租户空间，实现部门知识库自然物理隔离。
2. **员工关系映射（TenantMember）**：按企微人员所属部门自动分配空间读写/管理权限，支持跨部门与全员公共空间。
3. **企微扫码/网页免密登录（WeCom OAuth2）**：企微一键登录，直接置换 WeKnora JWT Token 自动跳转进站。
4. **架构零侵入**：通过外层 Nginx 与独立网关解耦，无需修改任何 WeKnora 官方源码，无缝支持 WeKnora 升级。

---

## 目录结构规划

```
platform-opc/
├── docs/deploy/              # 生产环境标准化部署目录 (SOP 手册、Compose、Env 模板、运维脚本)
│   ├── SOP.md                # 生产环境标准化部署指南 (步骤级手册)
│   ├── Dockerfile            # 生产环境多阶段构建 Dockerfile
│   ├── docker-compose.yaml   # 生产环境 Docker Compose 编排文件 (统一名称 platform-opc)
│   ├── config.example.yaml   # 生产环境基础拓扑配置模板
│   ├── platform-opc.env.example # 生产环境安全环境变量模板
│   └── scripts/              # 运维工具 (如管理员 Token 签发脚本)
├── config/                   # 配置加载与环境变量映射
├── pkg/                      # 核心服务包 (handler, auth, storage, deploy, kbridge, sync)
├── migrations/               # 版本化数据库迁移 SQL (000001~000017)
├── Dockerfile                # 根目录多阶段构建文件 (产物: platform-opc)
├── go.mod                    # Go 依赖定义
└── main.go                   # 服务主入口
```

---

## 生产部署指引

生产环境制品、镜像与服务名均统一命名为 **`platform-opc`**。详细的生产部署操作、密钥生成、数据库前置准备与 Nginx 反向代理配置，请参考文档：
👉 [Platform-OPC 生产环境部署标准作业程序 (SOP)](docs/deploy/SOP.md)


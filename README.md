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

### 为应用配置持久卷

先更新 Deploy-OPC，再更新 Platform-OPC 和 dsh-plugin-opc。当前分配在 Deploy 所在的 Swarm 节点完成，应用固定调度到该节点。

1. 在该节点准备专用目录。例如用 `sudo install -d -m 0755 /data/opc-volumes` 创建本地卷的根目录。接入阿里云 OSS 时，用 [ossfs](https://www.alibabacloud.com/help/en/oss/developer-reference/ossfs-overview/) 将企业已建好的桶或前缀挂到独立目录，例如 `/mnt/company-oss`。用 `findmnt -T /mnt/company-oss` 确认实际挂载，并确保 Deploy 及应用运行用户可读写。OSS 凭据由节点上的 ossfs 管理。SQLite 和需要文件锁的工作负载使用本地磁盘。
2. 把专用目录以相同路径暴露给 Deploy 服务，并通过 `OPC_APP_VOLUME_ROOTS` 列出允许分配的根。只添加现场实际准备好的目录。以下以同时准备两种后端为例，旧环境请替换成实际的 Deploy 服务名。

   ```sh
   sudo docker service update \
     --mount-add type=bind,source=/data/opc-volumes,target=/data/opc-volumes \
     --mount-add type=bind,source=/mnt/company-oss,target=/mnt/company-oss \
     --env-add OPC_APP_VOLUME_ROOTS=/data/opc-volumes,/mnt/company-oss \
     deploy-opc
   ```

   先挂载 OSS，再执行服务更新。宿主机重新挂载 OSS 后，重建 Deploy 任务让它绑定新的挂载，再恢复应用部署。重新创建 Deploy 服务时同样要带上这些挂载和环境变量。

3. 在 Platform 的 `/admin/deploy` →「持久化存储与卷治理」→「配置存储」登记本地磁盘目录或阿里云 OSS。填写 Deploy 节点 ID、宿主机上的根目录。OSS 资源另填实际挂载的桶和前缀，如 `oss://company-app-data/opc`。启用资源，按需设为默认。保存时 Deploy 会核对节点、路径和文件系统。
4. 在 dsh-plugin-opc 发布需要持久化目录的应用时，为每个目录选资源。例如把应用容器的 `/app/data` 映射到资源根下的 `<应用名>-<应用ID>/data`。应用详情显示分配路径。已有绑定在升级时复用。更换根目录需先迁移数据。

应用发布归档使用 Platform 的 `S3_ENDPOINT`、`S3_ACCESS_KEY`、`S3_SECRET_KEY` 和 `S3_BUCKET`。默认桶 `opc-apps` 继续归档各版本的构建包、技能包、Dockerfile 和清单。旧版命名卷与 MinIO 分桶绑定仍可被已有应用读取。


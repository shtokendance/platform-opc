---
document: hotfix
hotfixId: HF-20261127-01
status: confirmed
baseBranch: release
baseCommit: d70f494f505557a79829242e6351cbee01823f3e
architectureImpact: none
---

# WorkOPC 版本安装包链接

版本列表在已发布或已归档的记录上提供「复制下载链接」，按该版本拥有的安装包列出 Mac（M 系列）、Mac（Intel）和 Windows（x64）。服务按版本记录 ID 读取安装包，生成包含版本、架构和扩展名的文件名，拒绝草稿、已撤回记录和不存在的安装包。顶部固定下载地址与更新 Feed 继续提供原有接口。

用户确认「按**按 Hotfix 做**」，并指示「不用做验证 直接修改上线上环境」。本轮按要求跳过专项测试、测试环境和独立审查，仅编译镜像所需的 Linux amd64 二进制。生产范围是 `10.101.103.129` 的 `platform-opc-gateway`，使用当前镜像的运行层及现有 Compose 配置。新增公开下载路由沿用 Platform 的发布状态、存储和 Range 处理，无数据迁移。

生产回滚使用既有的 `platform-opc:d70f494f`，按相同 Compose 配置仅重建 `platform-opc-gateway`。生产临时镜像使用独立标签，正式 Git 发布清单仍指向对应的已提交镜像。后续按常规源码发布时应先纳入此修复，避免覆盖线上改动。

本地 Linux amd64 构建完成。生产已将 `platform-opc-gateway` 切至 `platform-opc:workopc-download-links-hf-20261127-01`（`sha256:df96a0c1653bddbe2843d1814f67d4f03c81455bef0327f21b8f8be143cb78a4`），沿用部署前的环境和配置挂载，服务启动后 `/health` 返回 `ok`。保留原镜像用于回滚。

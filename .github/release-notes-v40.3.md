# v40.3 — 稳定对象身份与离线同步基础

- 文字、文件和链接使用与文件名、路径无关的永久 UUID。
- API 返回递增的 `revision`；过期写入返回 HTTP 409 和服务器当前版本。
- 重命名、编辑、收藏、删除支持 `expectedRevision`，并继续兼容旧路径 ID。
- 普通写操作支持持久化 `Idempotency-Key`，客户端重试不会重复执行。
- SSE 和网页卡片统一使用 UUID 与 revision。
- 现有数据原地懒迁移，不移动或重命名载荷文件。
- Android 1.0.59 增加按服务器隔离的 SQLite、本地 outbox、持久上传队列、联网重试和冲突处理。

镜像：

- `ghcr.io/juddd/local-content-share:latest`
- `ghcr.io/juddd/local-content-share:v40.3`

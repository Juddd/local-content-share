# v40.4 — 稳定身份迁移完善

- 统一链接的内部存储表示，自动合并早期转义路径映射且保持原 UUID。
- 删除已不存在的 UUID 对象返回幂等成功，离线删除不会无限重试。
- 过期自动删除同步清理 UUID 与收藏元数据。
- Android 1.0.60 保留旧版下载 URI，系统分享接入持久队列与联网后台补交。

镜像：

- `ghcr.io/juddd/local-content-share:latest`
- `ghcr.io/juddd/local-content-share:v40.4`

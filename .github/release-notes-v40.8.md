# 内容生命周期与文件事务模块

- 内容 UUID、revision、收藏和创建/修改时间统一由内容生命周期模块原子提交。
- 结构化 SSE 的 sequence、历史补发、断档对账和慢客户端处理集中到同一事件模块。
- 流式上传、网页表单上传和 URL 下载统一进入文件传输模块，不再维护三套临时文件逻辑。
- 文件先写入隔离的临时区和持久 journal，再提交内容元数据并原子发布；中断后可在重启时恢复。
- 上传幂等结果持久保存 24 小时，服务重启后同一 Idempotency-Key 仍不会重复创建文件。
- 保持现有 REST、JSON/SSE、页面交互、数据目录和旧元数据兼容；旧 metadata 首次启动自动迁移。

## Android APK

Release 附带原生产证书签名的 `content-transfer-v1.0.66.apk`，可覆盖升级。

- Android 离线操作、系统分享、前台上传和 JobScheduler 统一使用一个同步传输引擎。
- 修复前台与后台同时接手待上传任务时，界面可能停留在“等待处理”的竞态。
- SHA-256：`32e3fda957fc891841fafd25cdcbd763347608c6592ee32910807fcc1b24476b`

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.8`
- `ghcr.io/juddd/local-content-share:latest`

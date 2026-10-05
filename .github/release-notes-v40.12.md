# Local Content Share v40.12

- 文字区新增“普通”和“私”两个分类，默认显示普通卡片。
- 文字卡片右下角增加锁图标，可直接在普通与私分类之间切换；收藏星星保持原有位置和行为。
- 服务端持久化私分类状态，并在重命名、迁移、删除和稳定 UUID 映射时保持元数据一致。
- Android 离线操作队列支持私分类切换，联网后自动同步并遵循 revision 冲突检查。
- 网页端和 Android 端的分类筛选、搜索、SSE 局部更新保持一致。

本版本不修改现有数据目录中的内容；已有文字卡片默认归入“普通”。

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.12`
- `ghcr.io/juddd/local-content-share:latest`

## 主要更新

- SSE 从字符串通知升级为带连续序号的结构化事件。
- 新增、修改、重命名和删除均推送精确条目或 ID。
- 网页与 Android 在线时进行局部更新，不再为每次变化刷新完整列表。
- SSE 首次连接、断线重连、App 恢复前台或发现事件序号断档时执行全量对账。
- 保留 REST API 负责增删改查和文件传输。
- Files 新增和删除继续使用局部动画，操作体验更连贯。

## 同步协议示例

```json
{
  "sequence": 1842,
  "type": "deleted",
  "id": "files/example.apk"
}
```

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.0`
- `ghcr.io/juddd/local-content-share:latest`

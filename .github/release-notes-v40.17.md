# Local Content Share v40.17

- 网页端切到后台或离开页面时关闭 SSE 实时连接，释放浏览器同源连接配额；页面恢复后重新订阅并按事件序号补收变更。
- 页面锁定时关闭全部实时连接，避免后台重连绕过锁定状态。
- 增加网页实时连接生命周期回归测试，并纳入 GitHub Actions。

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.17`
- `ghcr.io/juddd/local-content-share:latest`

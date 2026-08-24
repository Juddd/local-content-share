# 设备中心网络判断修复

- 设备中心的局域网/外网判断改为探测 NAS 实际内网入口，与 Android 首页的 DNS 分流语义保持一致。
- 浏览器无法访问内网入口时最多等待 3 秒，随后明确标记为“外网”，不再长期停留在未知状态。
- 不再把 `127.0.0.1`、Docker 网关或反向代理地址显示成浏览器 IP，也不会据此误判为局域网。
- 增加浏览器私网探测预检支持；无法可靠取得客户端地址时显示“地址未知”，不再展示虚假地址。
- 内网探测地址由服务端网卡动态发现，不写死个人 NAS 地址。

## Android APK

本次没有 Android 代码变化，现有 App 会直接读取服务端的新判断结果，无需升级。

Release 继续附带兼容的 `content-transfer-v1.0.64.apk` 供新安装使用。

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.7`
- `ghcr.io/juddd/local-content-share:latest`

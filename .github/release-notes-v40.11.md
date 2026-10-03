# Local Content Share v40.11

- 链接区不再限制为 `http://` 或 `https://` 开头。
- 支持 `magnet:`, `thunder:`, `ed2k:`, `mailto:`, `ftp:` 及其它符合 URI 协议格式的链接。
- 网页端和动态局部更新卡片统一保留合法 URI，并继续拦截 `javascript:`, `vbscript:` 和 `data:` 协议。
- Android 1.0.71 同步使用相同的链接协议校验规则。

本版本不修改现有数据目录中的内容。

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.11`
- `ghcr.io/juddd/local-content-share:latest`

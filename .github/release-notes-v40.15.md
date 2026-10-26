# Local Content Share v40.15

- 修复 PDF 文件下载后扩展名错误的问题。
- 文件下载接口现在使用真实文件名和扩展名，不再把稳定 UUID 当作下载文件名。
- 网页端文件下载保存时使用文件卡片中的原文件名。
- URL 下载会根据响应 MIME 类型和实际文件头自动校正文件扩展名，同时保留用户明确指定的文件名。
- 预览链接保存文件时会保留原始文件名。

## 容器镜像

- `ghcr.io/juddd/local-content-share:v40.15`
- `ghcr.io/juddd/local-content-share:latest`

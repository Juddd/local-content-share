## v39.8

- 服务端新增 multipart 流式文件上传 `/upload-stream`，边读边写临时文件并原子落盘。
- 文件及 URL 下载大小上限统一提高至 4 GiB。
- 网页与 Android 上传显示实时进度；Android 分享上传统一缓存并支持失败重试。
- 新增流式上传单元测试，发布前由 GitHub Actions 执行 `go test ./...`。

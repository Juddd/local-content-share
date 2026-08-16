# Local Content Share 个人增强版

本版本基于官方 `Tanq16/local-content-share` 制作，主要针对多设备文字、文件和链接中转场景进行界面与操作增强。

对应个人增强版源码分支：`Juddd/local-content-share:main`。

## 新增与改进

- Snippets 卡片直接显示标题与正文
- 正文预览限制为前 600 个 Unicode 字符，超长内容点击查看全文
- 复制 Snippet 时始终复制完整正文
- 桌面端自动利用横向空间并动态增加卡片列数，手机端保持单列
- 文件区域增加直达 URL 复制按钮，自动采用当前访问域名
- Links 支持独立标题、标题重命名和新标签页打开
- 兼容官方旧版仅保存 URL 的 Links 数据
- 修复 HTTP 环境复制内容时页面跳到最底部的问题
- 保留官方的数据目录结构，可直接挂载原有 `/app/data`

## 容器镜像

```text
ghcr.io/juddd/local-content-share:v38-yode.1
```

同时提供 `latest` 标签。镜像支持 `linux/amd64` 和 `linux/arm64`。

## 升级提示

升级前建议备份数据目录。只要继续把原数据目录挂载到 `/app/data`，升级容器不会删除已有的 Snippets、Files、Links 或 Notepad 内容。

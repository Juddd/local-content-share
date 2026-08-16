# Local Content Share v39

本版本基于官方 v38，由 Juddd 针对多设备文字、文件、图片和链接中转场景集中增强。此前个人版的全部修改现统一收录为 v39。

## 内容展示与操作

- Snippets 卡片同时显示标题和正文，正文默认展示前 600 个 Unicode 字符，超长内容点击后查看全文。
- 复制 Snippet 时始终复制完整正文，并修复 HTTP 环境下点击复制导致页面跳到底部的问题。
- Files 增加直达地址复制按钮，地址自动使用当前浏览器访问的局域网或外网域名。
- Links 支持独立标题、重命名、复制地址以及在新标签页打开。
- 兼容官方旧版仅保存 URL 的 Links 数据。

## 界面布局

- 桌面端充分利用横向空间，按窗口宽度自动增加卡片列数。
- 手机端保持适合窄屏操作的单列布局。
- Snippets、Files 和 Links 均保留各自清晰的区域标题。

## 独立排序

- Snippets、Files、Links 各自拥有独立排序按钮。
- 每个区域均可按创建时间、修改时间或标题字典序排序。
- 再次点击当前条件即可切换升序和降序。
- 三个区域的排序偏好分别保存在当前浏览器中，互不影响。
- 创建和修改时间持久化保存，重命名或编辑不会丢失时间信息。

## 容器与兼容性

- 镜像：`ghcr.io/juddd/local-content-share:v39`
- 同步更新：`ghcr.io/juddd/local-content-share:latest`
- 支持 `linux/amd64` 和 `linux/arm64`。
- 保留官方 `/app/data` 数据目录结构，升级不会删除已有 Snippets、Files、Links 或 Notepad 内容。

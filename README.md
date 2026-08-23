<div align="center">
  <img src="assets/logo.svg" alt="Local Content Share 标志" width="200">
  <h1>Local Content Share（个人增强版）</h1>

  <a href="https://github.com/Juddd/local-content-share/actions/workflows/docker-publish.yml"><img alt="容器构建状态" src="https://github.com/Juddd/local-content-share/actions/workflows/docker-publish.yml/badge.svg"></a>
  <a href="https://github.com/Juddd/local-content-share/releases"><img alt="GitHub Release" src="https://img.shields.io/github/v/release/Juddd/local-content-share"></a>
  <br><br>
  <a href="#功能">功能</a> &bull; <a href="#本增强版新增内容">增强内容</a> &bull; <a href="#docker-部署">Docker 部署</a> &bull; <a href="#使用说明">使用说明</a>
</div>

---

Local Content Share 是一个简洁的自托管内容中转服务，可以在不同设备之间共享文字片段、文件和链接。客户端无需安装应用，只要使用浏览器打开服务页面即可。

本仓库 Fork 自 [Tanq16/local-content-share](https://github.com/Tanq16/local-content-share)，在保留官方功能和数据格式的基础上，针对桌面端横向空间、文字直接预览、文件地址分享及链接管理进行了增强。

> [!WARNING]
> 本项目没有用户认证和权限系统。若直接暴露到公网，任何能够访问地址的人都可能查看、上传或删除内容。请根据实际需要在反向代理层增加认证与访问限制。

## 功能

- 在浏览器中创建、查看、编辑、重命名、复制和删除文字片段
- 上传、预览、下载、重命名和删除文件
- 保存、复制、重命名及打开常用链接
- 内置支持 Markdown 编辑和预览的 Notepad
- 支持拖放、选择和粘贴上传文件或截图
- 支持永不过期、1 小时、4 小时、1 天及自定义有效期
- 使用 SSE 在多个浏览器之间同步内容变化
- 静态资源完全本地化，局域网断网时仍可使用
- 自动跟随系统浅色/深色主题
- 支持浏览器访问和 PWA 安装
- 提供 AMD64 与 ARM64 容器镜像

## 本增强版新增内容

- Snippets 卡片同时显示标题和正文，不再只能点击后查看内容
- 正文最多直接预览前 600 个 Unicode 字符，超长内容点击正文查看全文
- 复制按钮始终复制完整正文，不受预览截断影响
- 桌面端取消固定内容宽度，卡片按屏幕宽度自动增加列数；手机端保持单列
- 调整 Snippets 卡片结构，正文可以使用按钮下方的完整宽度
- Files 增加“复制文件直达地址”按钮，地址会自动使用当前访问域名
- Links 支持独立标题、标题重命名以及在新标签页打开
- 兼容旧版只保存 URL 的 Links 数据
- 修复 HTTP 环境下复制操作导致页面跳到最底部的问题
- Snippets 和 Files 按创建时间倒序排列，最新添加的内容显示在最前面
- 顶部排序菜单可按创建时间、修改时间或标题切换，并支持升序/降序

## Docker 部署

推荐使用本仓库发布到 GitHub Container Registry 的镜像：

```yaml
name: local-content-share

services:
  local-content-share:
    image: ghcr.io/juddd/local-content-share:latest
    container_name: local-content-share
    restart: unless-stopped
    ports:
      - "8084:8080"
    volumes:
      - /volume1/docker/local-content-share/data:/app/data
    environment:
      TZ: Asia/Shanghai
```

启动项目：

```bash
docker compose up -d
```

服务默认监听容器内的 `8080` 端口。上面的示例会通过宿主机 `8084` 端口访问。

数据全部保存在挂载到 `/app/data` 的目录中。更新或重建容器不会删除这些数据。

### 从源码构建

```bash
git clone https://github.com/Juddd/local-content-share.git
cd local-content-share
docker build -t local-content-share .
```

## 使用说明

Android 客户端源码和独立构建说明位于 [Juddd/local-content-share-android](https://github.com/Juddd/local-content-share-android)，Release 提供使用原生产证书签名、可覆盖升级的 APK。

### 稳定 ID 与并发写入

`/api/v1/items` 和结构化 SSE 事件中的 `id` 是永久 UUID，`storageId` 是当前物理路径，`revision` 是递增修订号。重命名只修改 `storageId`，不会改变 `id`。

编辑、收藏、重命名和删除时可以提交 `expectedRevision`。如果服务器内容已经变化，接口返回 HTTP 409，并在 JSON 的 `item` 字段中提供当前版本。需要安全重试的写请求应携带唯一的 `Idempotency-Key` 请求头。

### 文字片段

点击页面顶部的 `New`，可填写可选标题和正文。未填写标题时会自动使用时间作为名称。

- 点击铅笔按钮编辑正文
- 点击光标按钮修改标题
- 点击复制按钮复制完整正文
- 超过 600 个字符时，点击正文打开全文窗口
- 点击垃圾桶按钮删除内容

### 文件

点击 `New` 后选择文件，也可以把一个或多个文件拖入上传区，或者直接粘贴剪贴板中的文件和截图。

- 下载按钮保存文件
- 眼睛按钮由浏览器直接预览文件
- 链条按钮复制当前访问环境下的文件直达地址
- 光标按钮重命名文件

如果局域网和公网对同一域名使用不同 DNS 解析，复制出来的地址会自动匹配当前访问环境。

### 链接

点击顶部的 `Link`，填写标题和以 `http://` 或 `https://` 开头的 URL。

- 光标按钮修改标题，不改变 URL
- 跳转按钮在新标签页打开链接
- 复制按钮复制 URL
- 旧版链接会继续兼容，并可通过重命名补充标题

### 设备中心与远程锁定

Android App 的“设置 → 设备中心”会列出打开过网页的浏览器设备，并显示在线/后台/离线状态、IP、页面数量和最后活动时间。设备可以单独重命名，也可以远程“关闭并锁定”或解除锁定。

浏览器身份由服务端随机生成，保存在同一浏览器配置文件共享的长期 HttpOnly Cookie 中，不使用浏览器指纹。Chrome 与 Firefox、普通与无痕窗口、清理 Cookie 后都会被视为不同设备。

锁定时网页会先尝试 `window.close()`；浏览器不允许关闭普通标签页时，服务端改为返回真正的 HTTP 404。刷新、重新输入网址和同一浏览器配置文件中新开的标签页仍会保持 404，直到 App 解除锁定。设备名称与锁定状态保存在 `data/devices.json`，服务或 NAS 重启后不会丢失。

当前部署按单用户、可信网络设计，没有登录鉴权；能访问服务 API 的客户端也能调用设备控制接口。如将来对公网开放给不可信用户，应先增加认证。

### 有效期

创建文字或上传文件时，可以循环选择 `Never`、`1 hour`、`4 hours`、`1 day` 或 `Custom`。自定义值格式为“数字 + 单位”，例如：

- `30m`：30 分钟
- `6h`：6 小时
- `3d`：3 天
- `2w`：2 周
- `1M`：1 个月
- `1y`：1 年

也可以通过 `DEFAULT_EXPIRY` 环境变量设置默认有效期。

## 反向代理说明

反向代理可能限制上传大小或缓存上传请求。以 Nginx Proxy Manager 为例，可以使用：

```nginx
client_max_body_size 5G;
proxy_request_buffering off;
proxy_buffering off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
proxy_connect_timeout 3600s;
```

## 数据目录

应用会在 `/app/data` 下保存：

- `text/`：文字片段
- `files/`：上传文件
- `notepad/`：Markdown 记事本
- `links.file`：链接
- `expirations.json`：过期时间
- `devices.json`：浏览器设备名称与持久锁定状态

请确保容器对挂载的数据目录具有读写权限，并在升级前做好备份。

## 上游与许可证

- 个人增强版仓库：[Juddd/local-content-share](https://github.com/Juddd/local-content-share)
- 官方上游仓库：[Tanq16/local-content-share](https://github.com/Tanq16/local-content-share)

本项目继续遵循上游仓库所采用的许可证。

# CHENGJIAN 界面迁移与启动说明

交付版本：`chengjian-paper-20261021`（Windows，前端已嵌入 `chengjian.exe`）。

后续开发项目为 `E:\cyberagent\chengjian\project`。原 `E:\cyberagent\CHENGJIAN` 保留为参考。

## 启动与停止

- 双击工作区 `E:\cyberagent\quick-start-chengjian.cmd`，或本目录 `quick-start.cmd`，启动后自动打开浏览器。
- 地址：<http://127.0.0.1:8787/>；代理监听：`127.0.0.1:8788`。
- 首次访问需要在页面设置管理员密码。交付时尚未设置管理员密码。
- 双击工作区 `E:\cyberagent\quick-stop-chengjian.cmd`，或本目录 `quick-stop.cmd`，快速关闭 CHENGJIAN。也可以在本目录运行 `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\stop.ps1`。
- 工作区原有的 `start-chengjian.ps1`、`stop-chengjian.ps1`、`build-chengjian.ps1` 已转发到本项目；`start.bat` 也已转发到 CHENGJIAN 快速启动入口。
- 重复启动会复用已运行的 CHENGJIAN；端口由其他项目占用时会报错。停止脚本只结束本项目进程，保留共享 PostgreSQL。

运行配置固定使用本目录 `config.json`，技能目录固定为本目录 `skills`，数据与日志分别写入 `data`、`logs`。启动时清除继承的 `CHENGJIAN_PG_DSN`，避免旧环境覆盖 CHENGJIAN 数据库配置。

CHENGJIAN 使用独立数据库 `chengjian_chengjian`；原 CHENGJIAN 的任务、账号和模型设置没有自动迁入。当前机器所需数据库已创建。迁移到其他机器时，需要配置 PostgreSQL 并创建对应数据库，不能只复制 exe。

## 重新编译

先停止 CHENGJIAN，再双击本目录 `rebuild.cmd`，完成后重新启动。也可以执行：

```powershell
.\stop.ps1
.\build.ps1
.\start.ps1 -OpenBrowser
```

构建会依次生成静态前端、嵌入资源和 Windows 可执行文件，并更新 `chengjian.exe.sha256`、`build-info.json`。缺少前端依赖时自动执行 `npm ci`；需要强制重装可加 `-InstallDependencies`。需要下载代理时，可传 `-Proxy http://127.0.0.1:10808`。脚本优先使用工作区现有 Node.js，Go 通过本机工具链解析；构建期间不使用 CHENGJIAN 的本地 norma 替换。

## 迁移内容

以原 CHENGJIAN 当前工作目录的界面为参考，迁入纸色主题、字体、侧边栏、导航头、模块概览、状态和 Markdown 展示、仪表盘、任务与详情、发现与报告、资产、流量、指令、模型记录和系统页面样式。CHENGJIAN 独有内网页面接入同一套主题与模块概览，保留其原有入口和组件。

保留 CHENGJIAN 的登录刷新、SSE 票据、会话与内网相关逻辑。为仪表盘与报告展示补齐只读遥测和报告链重建接口，以及嵌入静态资源的缓存服务。此次没有迁移攻击执行、隧道执行或 planner-worker 调度代码。

合并前备份位于 `E:\cyberagent\.local\chengjian-style-backup-20261020`，其中 `migration.json` 记录前端文件清单和手工合并情况。原 CHENGJIAN 项目文件未因本次迁移而改写。

## 验证范围

- 前端 TypeScript 检查通过；生产构建静态导出 32 个路由。
- 所有 Go 包通过编译检查（`go test ./... -run '^$'`，不代表完整测试套件已运行）。
- 报告链单元测试，以及仪表盘、静态资源服务相关测试通过。
- 登录刷新、安全链接、报告解析共 17 项 Node.js 测试通过。
- 浏览器遍历 22 个页面，并检查移动端、深色模式、报告样例与首次设置页，共 31 项检查；没有页面脚本异常、HTTP 错误或意外修改请求。
- 启动、重复启动、停止、环境隔离、运行中禁止覆盖二进制、文件校验和均已验证；8 个 PowerShell 脚本语法检查通过。

浏览器结果见 `screenshots/paper-migration/results.json`，同目录保留截图。报告截图使用只读样例；本次没有创建或执行渗透任务，没有验证真实攻击流程，也不承诺不存在任何潜在缺陷。

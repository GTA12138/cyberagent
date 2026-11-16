# 澄鉴 CHENGJIAN

源码与程序目录：`E:/cyberagent/chengjian/project`。

- 双击 `quick-start.cmd`：启动并打开网页。
- 双击 `quick-stop.cmd`：正常关闭程序，共用 PostgreSQL 保持运行。
- 双击 `quick-status.cmd`：查看运行状态。
- 运行 `project/build.ps1`：重建前端和后端，并刷新 SHA-256 与构建信息。

访问地址：<http://127.0.0.1:8787/>。

登录用户名：`CHENGJIAN`；原有登录密码继续有效。

程序名为 `chengjian.exe`，数据库与项目登录角色为 `chengjian`。配置、JWT 密钥和业务数据继续放在程序目录中的 `config.json`、`jwt.key`、`data/`。环境变量统一使用 `CHENGJIAN_` 前缀。

原源码、程序、配置、数据库备份位于 `reports/2026-10-27-fixes/`。该目录包含敏感备份，仅用于本机恢复，不要上传或公开。历史材料中的旧名称保留原样。

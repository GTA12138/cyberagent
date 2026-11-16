# CHENGJIAN 全链路靶场（外网 → 立足点 → 内网纵深）

零虚拟化方案：用 Windows 的 `127.0.0.x` 多回环地址模拟多台主机（与项目文档里
红日 3 验收环境同思路）。全部进程都是本机普通进程，不需要 BIOS 虚拟化、不需要
WSL / Docker。

```
┌─ 外网 DMZ（127.0.0.1:8080，PHP 8.5 内置服务器）─────────────────┐
│  /                         ACME 门户（首页注释提示 /backup 待删）│
│  /robots.txt               指向 /backup/ 与 /admin.php           │
│  /backup/README.txt        泄露后台账号 + 上传目录 + DSN         │
│  /backup/site-20261001.zip 站点备份（config/db.php 同一套凭据）  │
│  /login.php                后台登录（portal_user / Portal_2026!）│
│  /admin.php?sid=…          部署面板（上传表单 + 已上传清单）     │
│  /upload.php?sid=…         补丁上传：不校验扩展名 → 落地即执行   │
└──────────────────────────────┬──────────────────────────────────┘
                               │ register_session(http_php) 立足点
                               ▼
                    平台（任务级 MITM → 隧道 → 内网）
                               │
        ┌──────────────────────┴───────────────────────┐
        ▼                                              ▼
 127.0.0.2:2222  内网主机A（SSH，paramiko）      127.0.0.3:8000  内网主机B
 root/chengjian123、deploy/deploy2026            /secret/flag = FLAG-INTRANET-B-7f3a
 cat flag.txt → FLAG-INTRANET-A-2b91
```

## 预期打点链

1. **侦察**：`http://127.0.0.1:8080/` → 首页注释提到 `/backup`；`/robots.txt` 也指向它；
2. **信息泄露**：`/backup/README.txt` 给出后台账号 `portal_user:Portal_2026!`、
   上传目录 `/upload/`、以及 `/backup/site-20261001.zip`（zip 内 `config/db.php` 是同一套凭据）；
3. **登录**：`POST /login.php`（`?format=json` 返回 `sid`，也可用 cookie）→ 拿到 sid；
4. **上传**：`POST /upload.php?sid=<sid>`，`multipart/form-data`，字段名 `file`，
   响应 `{"ok":true,"url":"/upload/<文件名>"}`；
5. **立足点**：`register_session(kind=http_php, url=…/upload/x.php, password=cmd)`
   —— 上传的马落地即执行（PHP 内置服务器直接执行 `.php`）；
6. **内网侦察**：`session_recon` → 发现 `127.0.0.2` / `127.0.0.3` → `pending_scope` 人工审批；
7. **隧道**：`tunnel_probe` → `tunnel_deploy(adapter=chisel)`（平台 `data/tools/chisel.exe`
   已就位，经 stage 投递到目标，目标回连 `127.0.0.1`）；
8. **横向**：`lateral_exec(method=ssh, target=127.0.0.2:2222)` → 弱口令 →
   `cat flag.txt` → `FLAG-INTRANET-A-2b91`，且**退出码真实**（未知命令 127、`false` 为 1）；
9. **纵深证据**：经隧道 `curl http://127.0.0.3:8000/secret/flag` → `FLAG-INTRANET-B-7f3a`。

## 启动 / 停止 / 自检

```powershell
powershell -ExecutionPolicy Bypass -File E:\cyberagent\chengjian\lab\start-lab.ps1            # 启动
powershell -ExecutionPolicy Bypass -File E:\cyberagent\chengjian\lab\start-lab.ps1 -Stop      # 停止
powershell -ExecutionPolicy Bypass -File E:\cyberagent\chengjian\lab\start-lab.ps1 -Status    # 状态
powershell -ExecutionPolicy Bypass -File E:\cyberagent\chengjian\lab\start-lab.ps1 -Verify    # 全链路自检
```

自检会真的走一遍「首页 → 真 404 → 泄露点 → 登录 → 上传 → 马执行 → SSH 认证/退出码
→ 数据站 flag」，19 项全绿才算靶场可用。自检自己上传的探针文件用完即删。

也可以直接调 `labctl.py`（`start` / `stop` / `status` / `verify` / `serve`）；
`serve` 是前台托管模式，本进程活着靶场就活着，适合在受限环境里跑。

## 平台侧配套

- 任务 scope：初始 `127.0.0.1/32`，内网段走 `pending_scope` 审批（这就是人工闸门本身）；
- `CHENGJIAN_CALLBACK_ADDR=127.0.0.1`（本机拓扑，目标回连平台即回环）；
- LLM 需先在 UI 配置，否则 worker 会以 `no available fallback LLM provider` 收场；
- **Windows 立足点需要平台能认出平台形态**：`session/http_shell.go` 的 PHP 探针现在会
  读 `PHP_OS_FAMILY` 并把会话标成 windows，否则隧道会去投 ELF 版 chisel 而必然失败。
  改完需要 `project\build.ps1 -SkipFrontend` 重建 `chengjian.exe` 才生效。

## 这一版修了什么（对照上次跑不通的现场）

| 上次的现象 | 根因 | 这一版 |
|---|---|---|
| 目录扫描全部 200/210B，`/adminer.php` 也回首页 | PHP 内置服务器把未知路径回落到 `index.php` | 加 `dmz/router.php`：存在的文件交给服务器，其余**真 404** |
| 找到 `/upload` 但 POST 回来 `403 Forbidden`（200 状态） | 那只是 `upload/index.html` 里写死的静态文案，根本没有上传接口 | 真接口 `/upload.php`（登录态校验 + multipart + 落地即执行） |
| `/backup/readme.txt` 读到一半线索就断了 | 泄露文案指向的 `/adminer.php` 是死胡同（无 DB、无 PUT 面） | 删掉 adminer；泄露点直接给可用的后台账号与上传目录 |
| 内网 SSH 认证成功但 exec 挂起 | asyncssh 服务端与 Go x/crypto/ssh 的通道时序不兼容 | 换成 paramiko 服务端，`go_ssh_test` 实测认证/回显/退出码全通 |
| 大量 `head`/`2>/dev/null`/`ls -la` 报错 | 平台的 Bash 工具在 Windows 上走 PowerShell | 靶场侧不做假设（文档写明），平台侧见上节 |

## 已验证

| 项 | 状态 |
|---|---|
| 首页 / 未知路径 404 / robots / 泄露点 / zip | ✅ 自检覆盖 |
| 登录（错口令 401、泄露凭据可登录、未登录被挡） | ✅ 自检覆盖 |
| 上传 → 马执行 PHP（平台 eval 探针 S42E）→ `shell_exec` 可用 | ✅ 自检覆盖 |
| SSH：弱口令 / 错误口令拒绝 / 真实退出码（0/1/127） | ✅ paramiko 客户端 + **Go x/crypto/ssh 客户端**双验证 |
| 数据站 `/secret/flag` | ✅ 自检覆盖 |
| chisel 隧道 | ⚠️ 二进制已就位（`data/tools/chisel.exe`），但需上面的平台重建才能在 Windows 立足点上部署 |

## 目录

```
lab/
├── labctl.py              启停 + 全链路自检（start/stop/status/verify/serve）
├── start-lab.ps1          薄封装：挑带 paramiko 的 python 后转交 labctl.py
├── dmz/                   外网 PHP 站
│   ├── router.php         真 404 路由（关键：没有它一切扫描都不可信）
│   ├── index.php login.php admin.php upload.php logout.php
│   ├── inc/config.php     站点配置（也是泄露内容）
│   ├── backup/            README.txt + site-20261001.zip
│   └── upload/            上传落地目录（.php 会被执行）
├── intranet_targets.py    内网两台主机（paramiko SSH + HTTP 数据站）
├── php/                   便携 PHP 8.5（含靶场专用 php.ini）
├── go_ssh_test/           平台同款 Go x/crypto/ssh 客户端（sshcheck.exe）
├── chisel.exe             隧道二进制（平台 data/tools/ 各有一份）
└── state/                 pid 台账与日志（运行时生成）
```

`downloads/`、`runtime/` 是早期准备 MariaDB 的残留，当前拓扑不用它们（后台登录走
PHP 自带会话，不连数据库）。

#!/usr/bin/env bash
# CHENGJIAN 安装脚本：① 全部 Docker  ② 本地编译运行
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker 环境检测 / 自动安装 ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "已检测到 docker 与 docker compose"; return
  fi
  warn "未检测到 docker / docker compose"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask '自动安装 Docker? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker 安装完成（用户组变更需重新登录后免 sudo）"
      else
        die "请自行安装 docker 后重试"
      fi ;;
    Darwin) die "macOS 请安装 Docker Desktop：https://www.docker.com/products/docker-desktop/" ;;
    *)      die "请自行安装 docker 后重试" ;;
  esac
}

# ── ① 全部 Docker ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres 密码（回车随机生成）' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY（可留空，后续在 UI 配）' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok "已生成 .env（POSTGRES_PASSWORD 已设置）"
  else
    info "沿用已存在的 .env"
  fi
  info "拉取镜像并启动…"
  docker compose pull || true
  docker compose up -d
  ok "启动完成 → http://localhost:8787"
  info "查看日志：docker compose logs -f chengjian"
}

# ── ② 本地编译运行 ──────────────────────────────
install_local(){
  echo "数据库安装方式："
  echo "  1) 连接已有 PostgreSQL"
  echo "  2) 用 Docker 起一个 PostgreSQL（需要 docker）"
  case "$(ask '选择' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres 密码（回车随机）' "$(rand)")"
      docker run -d --name chengjian-pg -p 5432:5432 \
        -e POSTGRES_USER=chengjian -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=chengjian \
        -v chengjian-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=chengjian DB_PASS="$pw" DB_NAME=chengjian DB_SSL=disable ;;
    *)
      DB_HOST="$(ask '数据库地址' 127.0.0.1)"
      DB_PORT="$(ask '端口' 5432)"
      DB_USER="$(ask '账号' chengjian)"
      DB_PASS="$(ask '密码' '')"
      DB_NAME="$(ask '数据库名' chengjian)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # 生成 config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "已生成 config.json"

  # go 环境检查
  command -v go >/dev/null 2>&1 || die "未检测到 Go，请先安装 Go（>=1.26）：https://go.dev/dl/"
  ok "Go: $(go version)"

  # 内嵌前端需要 node 出静态产物
  if command -v npm >/dev/null 2>&1; then
    info "构建前端静态产物…"
    ( cd web && npm ci && npm run build:static )
    # dist 整个被 gitignore,全新克隆下父目录可能不存在,先建再拷(2026-09 实测踩坑)
    mkdir -p server/webui/dist && rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "编译内嵌单二进制…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o chengjian ./cmd/chengjian
  else
    warn "未检测到 npm：将编译**不内嵌前端**的后端（前端需另跑 npm run dev）"
    CGO_ENABLED=0 go build -o chengjian ./cmd/chengjian
  fi
  ok "编译完成 → ./chengjian"

  # 可选：托管给 systemd（崩溃自动拉起 + 开机自启）。守护职责移交 systemd，
  # 不再套 start.sh；页面一键更新（退出码 75）由 Restart=on-failure 重新拉起。
  if [ "$(uname -s)" = Linux ] && command -v systemctl >/dev/null 2>&1; then
    if [ "$(ask '安装为 systemd 服务并开机自启? (y/n)' y)" = y ]; then
      install_systemd
      return
    fi
  fi

  info "启动…（Ctrl-C 退出）"
  ./chengjian
}

# ── ③ 可选：systemd 托管 ────────────────────────
install_systemd(){
  local svc_user="${SUDO_USER:-$(id -un)}"
  asroot(){
    if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi
  }
  if ! id -u "$svc_user" >/dev/null 2>&1; then
    svc_user="$(id -un)"
    warn "用户 ${SUDO_USER:-} 不存在，改用当前用户 $svc_user"
  fi

  # 环境变量模板：只在不存在时生成，避免覆盖已有配置
  if [ ! -f /etc/chengjian.env ]; then
    asroot tee /etc/chengjian.env >/dev/null <<'ENV'
# CHENGJIAN 环境变量（全部可选），修改后执行 systemctl restart chengjian 生效。
# 反弹 shell / 隧道回连平台用的地址（host:port）：
# CHENGJIAN_CALLBACK_ADDR=1.2.3.4:8787
ENV
    asroot chmod 600 /etc/chengjian.env
    ok "已生成 /etc/chengjian.env 模板"
  else
    info "沿用已存在的 /etc/chengjian.env"
  fi

  # 从模板生成 unit：替换安装目录与运行账号
  sed -e "s|^User=.*|User=${svc_user}|" \
      -e "s|^WorkingDirectory=.*|WorkingDirectory=${PWD}|" \
      -e "s|^ExecStart=.*|ExecStart=${PWD}/chengjian -addr :8787 -proxy :8788|" \
      packaging/chengjian.service | asroot tee /etc/systemd/system/chengjian.service >/dev/null
  ok "已安装 /etc/systemd/system/chengjian.service（User=${svc_user}, 目录=${PWD}）"

  asroot systemctl daemon-reload
  asroot systemctl enable --now chengjian
  ok "CHENGJIAN 已由 systemd 托管 → http://localhost:8787"
  info "查看日志：journalctl -u chengjian -f"
}

echo "=============================="
echo "  CHENGJIAN 安装"
echo "  1) 全部 Docker 安装"
echo "  2) 本地运行（go 编译）"
echo "=============================="
case "$(ask '选择' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "无效选择" ;;
esac

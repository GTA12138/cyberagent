#!/usr/bin/env python3
# intranet_targets.py — 内网两台"主机"（只有经隧道/内网路由才应可达）。
#
#   127.0.0.2:2222  内网"主机A" — SSH 服务端（paramiko），弱口令
#                   root/chengjian123、deploy/deploy2026
#                   exec 返回真实输出与**真实退出码**（lateral_exec method=ssh 的判据）
#   127.0.0.3:8000  内网"主机B" — HTTP 核心数据站，/secret/flag 是纵深达成证据
#
# 为什么从 asyncssh 换成 paramiko：平台侧横向走 Go 的 x/crypto/ssh，asyncssh 服务端
# 在这条链上认证后即挂起（通道 exec 请求不回、通道被提前关闭），也就是「打得到认证、
# 拿不到回显」。paramiko 的 server 端与 Go 客户端是长期互通的组合，通道语义（exec
# → stdout → exit-status → close）完全按 RFC 4254 走。
#
# 显式拒绝 shell/pty 以外的花样：靶标只承诺「exec 一条命令、拿回显和退出码」，
# 不假装提供交互式登录。
import os
import socket
import sys
import threading
import time

import paramiko

SSH_ADDR = ("127.0.0.2", 2222)
HTTP_ADDR = ("127.0.0.3", 8000)
SSH_USERS = {"root": "chengjian123", "deploy": "deploy2026"}

FLAG_A = "FLAG-INTRANET-A-2b91"
FLAG_B = "FLAG-INTRANET-B-7f3a"

PASSWD = """root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
deploy:x:1000:1000:deploy:/home/deploy:/bin/bash
"""

NOTES = """# 运维备忘
- 核心数据站已迁到 127.0.0.3:8000，只有内网能访问
- 取数接口 /secret/flag（别外传）
- 本机 flag 在 /root/flag.txt
"""

LOG_LOCK = threading.Lock()


def log(msg):
    with LOG_LOCK:
        sys.stderr.write(msg + "\n")
        sys.stderr.flush()


def split_chain(cmd):
    """把 `a && b; c` 拆成 [(op, cmd)]，只支持 && 与 ;（靶标不模拟完整 shell）。"""
    parts, buf, i, op = [], "", 0, None
    while i < len(cmd):
        if cmd.startswith("&&", i):
            parts.append((op, buf))
            buf, op, i = "", "&&", i + 2
        elif cmd[i] == ";":
            parts.append((op, buf))
            buf, op, i = "", ";", i + 1
        else:
            buf += cmd[i]
            i += 1
    parts.append((op, buf))
    return [(o, c.strip()) for o, c in parts if c.strip()]


def run_one(cmd, user):
    """执行单条命令，返回 (stdout, exit_code)。"""
    c = cmd.strip()
    for suffix in (" 2>&1", " 2>/dev/null"):
        if c.endswith(suffix):
            c = c[: -len(suffix)].strip()

    head, _, rest = c.partition(" ")
    head = head.strip()

    if head in ("whoami",):
        return user + "\n", 0
    if head in ("hostname",):
        return "HOST-A-CORP\n", 0
    if head in ("id",):
        return ("uid=0(root) gid=0(root) groups=0(root)\n" if user == "root"
                else "uid=1000(deploy) gid=1000(deploy) groups=1000(deploy)\n"), 0
    if head in ("pwd",):
        return ("/root\n" if user == "root" else "/home/deploy\n"), 0
    if head in ("uname",):
        return "Linux HOST-A-CORP 5.15.0-119-generic #129-Ubuntu SMP x86_64 GNU/Linux\n", 0
    if head in ("ls", "ll"):
        return "deploy.sh  flag.txt  notes.txt\n", 0
    if head in ("cat", "type", "more"):
        target = rest.strip().strip("'\"")
        if target in ("flag.txt", "/root/flag.txt", "/home/deploy/flag.txt"):
            return FLAG_A + "\n", 0
        if target in ("notes.txt", "/root/notes.txt"):
            return NOTES, 0
        if target == "/etc/passwd":
            return PASSWD, 0
        if target in ("deploy.sh", "/root/deploy.sh"):
            return "#!/bin/sh\ncurl -s http://127.0.0.3:8000/secret/flag > /tmp/.d\n", 0
        return "cat: %s: No such file or directory\n" % target, 1
    if head in ("ifconfig", "ip"):
        return ("eth0: flags=4163<UP,BROADCAST,RUNNING,MULTICAST>  mtu 1500\n"
                "        inet 10.0.0.2  netmask 255.255.255.0  broadcast 10.0.0.255\n"), 0
    if head == "echo":
        return rest + "\n", 0
    if head in ("true", ":"):
        return "", 0
    if head == "false":
        return "", 1
    if head in ("exit", "logout"):
        return "", 0
    if head in ("sudo",):
        return "User %s may run the following commands on HOST-A-CORP:\n    (ALL : ALL) NOPASSWD: ALL\n" % user, 0
    if head in ("systemctl", "service"):
        return "Unit not found.\n", 5
    if head in ("ps",):
        return "  PID TTY          TIME CMD\n    1 ?        00:00:02 systemd\n  842 ?        00:00:00 sshd\n", 0
    if head in ("netstat", "ss"):
        return ("tcp  0  0 0.0.0.0:22   0.0.0.0:*  LISTEN\n"
                "tcp  0  0 10.0.0.2:8000 0.0.0.0:*  LISTEN\n"), 0
    if head in ("wget", "curl"):
        return "bash: %s: 外网出口已封禁（仅内网可达）\n" % head, 1
    if head in ("python", "python3", "perl", "sh", "bash"):
        return "bash: %s: 靶标不提供脚本执行，仅支持常用查询命令\n" % head, 126
    return "sh: %s: command not found\n" % (c or head), 127


def run_command(cmd, user):
    out, code = "", 0
    for op, piece in split_chain(cmd):
        if op == "&&" and code != 0:
            break
        o, code = run_one(piece, user)
        out += o
    return out, code


class SSHSession(paramiko.ServerInterface):
    def __init__(self, peer):
        self.peer = peer
        self.user = None
        self.event = threading.Event()

    def check_auth_password(self, username, password):
        ok = SSH_USERS.get(username) == password
        log("[hostA-ssh] auth %s from %s: %s" % ("ok" if ok else "FAIL", self.peer, username))
        if ok:
            self.user = username
            return paramiko.AUTH_SUCCESSFUL
        return paramiko.AUTH_FAILED

    def get_allowed_auths(self, username):
        return "password"

    def check_channel_request(self, kind, chanid):
        if kind == "session":
            return paramiko.OPEN_SUCCEEDED
        return paramiko.OPEN_FAILED_ADMINISTRATIVELY_PROHIBITED

    def check_channel_exec_request(self, channel, command):
        cmd = command.decode("utf-8", "replace") if isinstance(command, bytes) else str(command)
        log("[hostA-ssh] exec %s: %s" % (self.user, cmd))

        def worker():
            try:
                # 关键时序：paramiko 是在 check_channel_exec_request 返回**之后**才给
                # 客户端回 CHANNEL_SUCCESS 的。这个线程如果立刻写数据/关通道，客户端
                # 会先收到 CHANNEL_CLOSE，于是在「等 exec 成功回包」时就抛
                # Channel closed —— 表现为「认证成功、exec 必失败」。
                time.sleep(0.05)
                out, code = run_command(cmd, self.user or "")
                if out:
                    channel.sendall(out.encode())
                channel.send_exit_status(code)
                channel.shutdown_write()  # EOF：客户端 read() 立即返回
                time.sleep(0.1)           # 让 exit-status/EOF 真正发出去再关
            except Exception as exc:  # noqa: BLE001
                log("[hostA-ssh] exec EXC: %s: %s" % (type(exc).__name__, exc))
            finally:
                try:
                    channel.close()
                except Exception:  # noqa: BLE001
                    pass

        threading.Thread(target=worker, daemon=True).start()
        return True

    def check_channel_shell_request(self, channel):
        # 不提供交互式 shell：lateral_exec 只需要 exec 通道，假装支持反而会让
        # 客户端挂在「等提示符」上。
        return False

    def check_channel_pty_request(self, channel, term, width, height, pixelwidth, pixelheight, modes):
        return True


def handle_client(conn, addr, host_key):
    peer = "%s:%s" % (addr[0], addr[1])
    try:
        transport = paramiko.Transport(conn)
        transport.add_server_key(host_key)
        transport.banner_timeout = 20
        server = SSHSession(peer)
        transport.start_server(server=server)
        # 保持连接直到客户端断开（exec 通道自行管理生命周期）
        while transport.is_active():
            transport.join(2)
    except Exception as exc:  # noqa: BLE001
        log("[hostA-ssh] conn %s EXC: %s: %s" % (peer, type(exc).__name__, exc))
    finally:
        try:
            conn.close()
        except Exception:  # noqa: BLE001
            pass


def serve_ssh():
    host_key = paramiko.RSAKey.generate(2048)
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(SSH_ADDR)
    srv.listen(32)
    log("[hostA-ssh] listening on %s:%d (users: %s)" % (SSH_ADDR[0], SSH_ADDR[1], ", ".join(SSH_USERS)))
    while True:
        conn, addr = srv.accept()
        threading.Thread(target=handle_client, args=(conn, addr, host_key), daemon=True).start()


# ---------- 内网主机 B：HTTP 核心数据站 ----------
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer  # noqa: E402

SITE_B = {
    "/": b"<html><body><h1>CORP Internal Data Site</h1><p>internal only</p></body></html>",
    "/secret/flag": ('{"flag":"%s","note":"core data site reached"}' % FLAG_B).encode(),
    "/api/v1/health": b'{"status":"ok","service":"corp-data"}',
}


class DataSiteHandler(BaseHTTPRequestHandler):
    server_version = "corp-data/1.4"

    def do_GET(self):
        body = SITE_B.get(self.path)
        if body is None:
            body = b"not found\n"
            self.send_response(404)
        else:
            self.send_response(200)
        ctype = "application/json" if self.path.startswith("/api") or self.path == "/secret/flag" else "text/html"
        self.send_header("Content-Type", ctype + "; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        log("[hostB-http] %s" % (fmt % args))


def serve_http():
    httpd = ThreadingHTTPServer(HTTP_ADDR, DataSiteHandler)
    log("[hostB-http] listening on %s:%d (/secret/flag)" % (HTTP_ADDR[0], HTTP_ADDR[1]))
    httpd.serve_forever()


def main():
    threading.Thread(target=serve_ssh, daemon=True).start()
    threading.Thread(target=serve_http, daemon=True).start()
    log("[targets] pid=%d ready" % os.getpid())
    threading.Event().wait()


if __name__ == "__main__":
    main()

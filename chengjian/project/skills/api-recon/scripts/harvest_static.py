#!/usr/bin/env python3
"""
harvest_static.py <BASE_URL> <OUTDIR>

静态材料采集与理解。**命令与产出契约保持向后兼容**——调用方（api-recon skill）
不需要改任何流程，但这个脚本现在会：

  1. 解析 HTML 的 script/modulepreload → webpack/Vite manifest → 迭代下载全部 chunk
     （原有行为）；
  2. 顺带读 robots.txt / sitemap.xml 作为种子（零成本，通常白捡一批页面）；
  3. 识别并下载 **sourcemap**（`//# sourceMappingURL=`），用 sourcesContent 还原
     未压缩源码后**再抽一遍**——命中率远高于压缩 bundle；
  4. 识别并下载 **runtime-config / asset-manifest / routes.json** 一类配置文件，
     递归抽出基址（api_base / baseURL / gateway …），产出 configs.txt；
  5. 抽取时区分「能确定 method 的调用点」与「仅字面量路径」：
     - api_methods.txt：`METHOD /path`（fetch / xhr.open / axios.get|post …）
     - api_static.txt：所有 API 形态路径（兼容旧消费方）
     - configs.txt / sourcemaps.txt / sm_apis.txt：新增材料面

为什么改这个脚本而不是加一个新工具：agent 已经会跑它。能力长在既有动作上，
才是能力提升；多一个「记得去跑」的新脚本，只是多一个会被忘记的开关。

Stdlib only. TLS 校验关闭（内网/自签目标）。
环境变量 CHENGJIAN_UA：平台按任务稳定分配的 UA（worker 的 BashEnv 已注入）。
不读它就等于每次都用同一个自报家门的 UA，指纹一致性当场作废。
"""

import sys, os, re, ssl, json, urllib.request, urllib.parse
from concurrent.futures import ThreadPoolExecutor

CTX = ssl.create_default_context(); CTX.check_hostname = False; CTX.verify_mode = ssl.CERT_NONE

# 优先用平台注入的 UA；没有则用一个常见的浏览器 UA（而不是 "spa-api-recon" 这种自报名）。
UA = os.environ.get("CHENGJIAN_UA") or (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

MAX_WORKERS = int(os.environ.get("CHENGJIAN_HARVEST_WORKERS") or 8)
JS_LIMIT = 8 << 20          # 8 MiB：5 MiB 的 bundle 在真实 SPA 里很常见
MAP_LIMIT = 32 << 20
TEXT_LIMIT = 2 << 20


def fetch(url, limit=TEXT_LIMIT):
    try:
        req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "*/*"})
        with urllib.request.urlopen(req, context=CTX, timeout=25) as r:
            data = r.read(limit)
            return data, r.status, (r.headers.get("Content-Type") or "")
    except Exception as e:
        return b"", getattr(e, "code", 0), ""


def origin_of(u):
    p = urllib.parse.urlparse(u)
    return f"{p.scheme}://{p.netloc}"


def same_origin(base, u):
    a, b = urllib.parse.urlparse(base), urllib.parse.urlparse(u)
    return (a.scheme, a.netloc) == (b.scheme, b.netloc)


# ---- chunk manifest parsing -------------------------------------------------
def matched_block(s, end_brace_idx):
    """Walk back from a '}' index to its matching '{' and return the object body."""
    depth = 0; j = end_brace_idx
    while j >= 0:
        if s[j] == '}': depth += 1
        elif s[j] == '{':
            depth -= 1
            if depth == 0: return s[j:end_brace_idx+1]
        j -= 1
    return ""


def parse_chunk_maps(js_text):
    """Return list of (publicPath_hint, {id: hash}) for every `}[x]+".js"` map
    (webpack __webpack_require__.u) found in the text."""
    maps = []
    pubs = re.findall(r'(?:\.p|publicPath)\s*=\s*"([^"]*)"', js_text)
    for m in re.finditer(r'\}\[[A-Za-z_$]\]\s*\+\s*"\.js"', js_text):
        body = matched_block(js_text, m.start())
        pairs = re.findall(r'(\d+):"([0-9a-fA-F]{6,16})"', body)
        if pairs:
            maps.append((pubs, dict(pairs)))
    # Vite style: __vite__mapDeps map of "assets/xx.js"
    for fn in re.findall(r'"(assets/[^"]+\.js)"', js_text):
        maps.append((["/"], {"__vite__": fn}))
    return maps


def chunk_urls(base, js_text):
    """Yield absolute chunk URLs reconstructable from this file's manifest(s)."""
    origin = origin_of(base)
    out = set()
    for pubs, idmap in parse_chunk_maps(js_text):
        paths = pubs or ["/assets/", "/"]
        for cid, h in idmap.items():
            if cid == "__vite__":
                fn = h  # already "assets/xx.js"
                for pp in paths:
                    out.add(urllib.parse.urljoin(origin + "/", fn))
                continue
            for pp in paths:
                if pp.startswith("http"): bareorigin = ""; prefix = pp
                else: bareorigin = origin; prefix = pp if pp.startswith("/") else "/"+pp
                if not prefix.endswith("/"): prefix += "/"
                out.add(f"{bareorigin}{prefix}{cid}.{h}.js")
    return out


# ---- endpoint / route extraction -------------------------------------------
API_HINT = re.compile(r'/(?:api|rest|service|services|gateway|graphql|v\d|web|admin|backend|open|internal|manage|console)\b', re.I)
# 注意：这里不再把 .json/.map 当「非目标」丢掉——runtime-config.json、manifest.json、
# routes.json 正是价值最高的材料。只排除真正的静态资源。
ASSET_EXT = re.compile(r'\.(js|mjs|css|png|jpe?g|svg|gif|woff2?|ttf|eot|ico|mp4|webm|webp|pdf|zip|gz)(\?|$)', re.I)
CONFIG_EXT = re.compile(r'\.(json|ya?ml|map)(\?|$)', re.I)

Q = r'["\'`]'
RE_LITERAL = re.compile(Q + r'(/[A-Za-z0-9_\-./{}:$~]*)' + Q)
RE_CONCAT_HEAD = re.compile(r'["\'](/[A-Za-z0-9_\-./]+/)["\']\s*\+')
RE_FETCH_LITERAL = re.compile(r'\bfetch\s*\(\s*' + Q + r'([^"\'`]+)' + Q)
RE_FETCH_METHOD = re.compile(r'\bfetch\s*\(\s*' + Q + r'([^"\'`]+)' + Q + r'\s*,\s*\{[^{}]{0,400}?method\s*:\s*' + Q + r'([A-Za-z]+)' + Q)
RE_XHR_OPEN = re.compile(r'\.open\s*\(\s*' + Q + r'([A-Za-z]+)' + Q + r'\s*,\s*' + Q + r'([^"\'`]+)' + Q)
RE_VERB_CALL = re.compile(r'\b(?:axios|ky|got|superagent)\s*\.\s*(get|post|put|patch|delete|head|options)\s*\(\s*' + Q + r'([^"\'`]+)' + Q)
RE_CLIENT_CALL = re.compile(r'\b(?:request|http|api|client|service|apiClient|httpClient)\s*\.\s*(get|post|put|patch|delete)\s*\(\s*' + Q + r'([^"\'`]+)' + Q)
RE_SOURCEMAP = re.compile(r'(?://|/\*)[#@]\s*sourceMappingURL=([^\s*]+)')
RE_ROUTE = re.compile(r'(?:path|to|redirect|href)\s*:\s*' + Q + r'(/[^"\'`]*)' + Q)

CONFIG_KEYS = (
    "api_base", "apibase", "baseurl", "base_url", "api_url", "apiurl", "api_host",
    "endpoint", "graphql", "wsurl", "ws_url", "socketurl", "socket_url", "gateway",
    "backend", "publicpath", "service_worker", "serviceworker", "route_catalog",
    "service_catalog", "integration_spec",
)


def strip_comments(src):
    """去掉 // 与 /* */ 注释，保留字符串字面量内容（简化状态机，够用）。"""
    out, i, n, quote = [], 0, len(src), ""
    while i < n:
        c = src[i]
        if quote:
            out.append(c)
            if c == "\\" and i + 1 < n:
                out.append(src[i + 1]); i += 2; continue
            if c == quote: quote = ""
            i += 1; continue
        if c in "\"'`":
            quote = c; out.append(c); i += 1; continue
        if src.startswith("//", i):
            while i < n and src[i] != "\n": i += 1
            out.append("\n"); continue
        if src.startswith("/*", i):
            i += 2
            while i + 1 < n and not src.startswith("*/", i): i += 1
            i += 2; continue
        out.append(c); i += 1
    return "".join(out)


def norm_path(p):
    """相对路径归一：去 fragment、去 query（端点识别只看 path）。"""
    p = p.split("#", 1)[0].split("?", 1)[0]
    return p


def extract_methods(js_text):
    """抽取 method 可确定的调用点 → [(METHOD, path)]。"""
    code = strip_comments(js_text)
    out = []
    explicit = set()
    for u, m in RE_FETCH_METHOD.findall(code):
        explicit.add(norm_path(u))
        out.append((m.upper(), u))
    for u in RE_FETCH_LITERAL.findall(code):
        # fetch(url) 无 init，按规范就是 GET；但若同一 URL 已有显式 method，
        # 不能再补一条 GET——那会造出一个并不存在的端点。
        if norm_path(u) in explicit:
            continue
        out.append(("GET", u))
    for m, u in RE_XHR_OPEN.findall(code):
        out.append((m.upper(), u))
    for rex in (RE_VERB_CALL, RE_CLIENT_CALL):
        for m, u in rex.findall(code):
            out.append((m.upper(), u))
    return [(m, norm_path(u)) for m, u in out if u.startswith("/")]


def extract_endpoints(js_text):
    code = strip_comments(js_text)
    paths = set(RE_LITERAL.findall(code))
    paths |= set(RE_CONCAT_HEAD.findall(code))
    api, cfg = set(), set()
    for p in paths:
        p = norm_path(p)
        if CONFIG_EXT.search(p):
            cfg.add(p); continue
        if ASSET_EXT.search(p):
            continue
        if p.count('/') < 2 and not API_HINT.search(p):
            continue
        if API_HINT.search(p):
            api.add(p)
    return api, cfg


def extract_routes(js_text):
    code = strip_comments(js_text)
    out = set()
    for m in RE_ROUTE.findall(code):
        m = norm_path(m)
        if m and not ASSET_EXT.search(m) and not API_HINT.search(m):
            out.add(m)
    return out


def extract_config_bases(base_url, text, depth=0):
    """从 JSON/YAML-ish 文本里递归抽基址。按 key 语义匹配，不靠位置。"""
    found = set()
    if depth > 8 or len(text) > 4 << 20:
        return found
    try:
        doc = json.loads(text)
    except Exception:
        # 不是严格 JSON：退化为 key:"value" 正则，仍按 key 语义过滤。
        for k, v in re.findall(r'["\']?([A-Za-z0-9_\-]+)["\']?\s*[:=]\s*["\']([^"\']{2,300})["\']', text):
            if any(w in k.lower().replace("-", "_") for w in CONFIG_KEYS):
                u = urllib.parse.urljoin(base_url, v)
                if same_origin(base_url, u):
                    found.add(u.rstrip("/"))
        return found

    def walk(node, key=""):
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, k)
        elif isinstance(node, list):
            for v in node:
                walk(v, key)
        elif isinstance(node, str):
            if key and any(w in key.lower().replace("-", "_") for w in CONFIG_KEYS):
                u = urllib.parse.urljoin(base_url, node)
                if same_origin(base_url, u):
                    found.add(u.rstrip("/"))
    walk(doc)
    return found


def unpack_sourcemap(base_url, text):
    """返回 (源码文本拼接, 是否含 node_modules 依赖树线索)。"""
    try:
        sm = json.loads(text)
    except Exception:
        return "", False
    contents = [c for c in (sm.get("sourcesContent") or []) if isinstance(c, str) and c.strip()]
    deps = any("node_modules/" in (s or "") for s in (sm.get("sources") or []))
    return "\n".join(contents), deps


# ---- main -------------------------------------------------------------------
def seed_urls(base):
    """robots.txt / sitemap.xml 作为额外种子。零成本，通常白捡一批页面。"""
    origin = origin_of(base)
    seeds = set()
    robots, st, _ = fetch(origin + "/robots.txt", 256 << 10)
    if st == 200 and robots:
        for line in robots.decode("utf-8", "ignore").splitlines():
            if line.lower().startswith("sitemap:"):
                seeds.add(line.split(":", 1)[1].strip())
    queue = list(seeds)
    seen = set(queue)
    while queue and len(seen) < 12:
        sm_url = queue.pop()
        body, st, _ = fetch(sm_url, 4 << 20)
        if st != 200 or not body:
            continue
        txt = body.decode("utf-8", "ignore")
        for loc in re.findall(r"<loc>\s*([^<\s]+)\s*</loc>", txt):
            loc = loc.strip()
            if loc.endswith(".xml") and loc not in seen and len(seen) < 12:
                seen.add(loc); queue.append(loc)
            elif same_origin(base, loc):
                seeds.add(loc)
    return {s for s in seeds if not s.endswith(".xml")}


def main():
    if len(sys.argv) < 3:
        print("usage: harvest_static.py <BASE_URL> <OUTDIR> [--coverage]"); sys.exit(1)
    # --coverage：额外产出 coverage.json，回答「这轮静态收割到底覆盖了多少」。
    # 它**不**自动点 Tab / 弹窗 / 分页——那需要浏览器，本脚本是纯 HTTP 静态收割。
    # 与其假装覆盖了动态面，不如把「静态面覆盖到哪、哪块没碰到」如实报出来。
    want_coverage = "--coverage" in sys.argv
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    base, outdir = args[0], args[1]
    if not base.startswith("http"): base = "https://" + base
    jsdir = os.path.join(outdir, "js")
    matdir = os.path.join(outdir, "materials")
    os.makedirs(jsdir, exist_ok=True); os.makedirs(matdir, exist_ok=True)

    print(f"[*] entry: {base}")
    html, status, _ = fetch(base)
    open(os.path.join(outdir, "index.html"), "wb").write(html or b"")
    html_text = (html or b"").decode("utf-8", "ignore")
    origin = origin_of(base)

    # initial scripts from HTML
    srcs = set(re.findall(r'<script[^>]+src="([^"]+\.js[^"]*)"', html_text))
    srcs |= set(re.findall(r'(?:src|href)="([^"]*\.js[^"]*)"', html_text))
    # modulepreload / preload as=script
    srcs |= set(re.findall(r'<link[^>]+rel="(?:modulepreload|preload)"[^>]+href="([^"]+\.js[^"]*)"', html_text))
    seed = set()
    for s in srcs:
        seed.add(s if s.startswith("http") else urllib.parse.urljoin(base, s))

    pages = seed_urls(base)
    cov["pages_seeded"] = len(pages)
    if pages:
        print(f"[*] robots/sitemap seeds: {len(pages)}")

    print(f"[*] {len(seed)} scripts referenced in HTML")

    have = {}      # url -> local path (js)
    mats = {}      # url -> local path (map/json)
    page_hits = [] # sitemap 页面里内联出现的路径
    # 覆盖统计：只记事实（看到几个、拿到几个、失败原因），不做推断。
    cov = {"js_seen": set(), "js_ok": set(), "js_fail": {}, "mat_fail": {}, "pages_seeded": 0, "pages_fetched": 0}

    def local_name(url, subdir):
        fn = os.path.basename(urllib.parse.urlparse(url).path) or "index"
        lp = os.path.join(subdir, fn)
        # 同名冲突：加短哈希前缀，避免不同目录同名 chunk 互相覆盖
        if os.path.exists(lp) and url not in have and url not in mats:
            import hashlib
            lp = os.path.join(subdir, hashlib.md5(url.encode()).hexdigest()[:8] + "_" + fn)
        return lp

    def dl_js(url):
        cov["js_seen"].add(url)
        if url in have: return have[url]
        data, st, _ = fetch(url, JS_LIMIT)
        if st == 200 and data and not data[:15].lstrip().startswith(b"<"):
            lp = local_name(url, jsdir)
            open(lp, "wb").write(data); have[url] = lp; cov["js_ok"].add(url); return lp
        # 失败原因分三类，因为它们对「覆盖缺口」的含义完全不同：
        # 超限=我们主动放弃；非 200=服务端不给；返回 HTML=路由回落到首页（SPA 常见）。
        if data and data[:15].lstrip().startswith(b"<"):
            cov["js_fail"][url] = "returned_html"
        elif st != 200:
            cov["js_fail"][url] = f"http_{st}"
        else:
            cov["js_fail"][url] = "empty_or_too_large"
        return None

    def dl_mat(url):
        if url in mats: return mats[url]
        data, st, _ = fetch(url, MAP_LIMIT if url.endswith(".map") else TEXT_LIMIT)
        if not (st == 200 and data and not data[:15].lstrip().startswith(b"<")):
            cov["mat_fail"][url] = f"http_{st}" if st != 200 else "not_material"
        if st == 200 and data and not data[:15].lstrip().startswith(b"<"):
            lp = local_name(url, matdir)
            open(lp, "wb").write(data); mats[url] = lp; return lp
        return None

    with ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
        list(ex.map(dl_js, seed))

    # iteratively expand via chunk manifests (chunks reference more chunks)
    seen_urls = set(have.keys()); frontier = list(have.values())
    rounds = 0
    while frontier and rounds < 6:
        rounds += 1
        new_urls = set()
        for lp in frontier:
            try: txt = open(lp, encoding="utf-8", errors="ignore").read()
            except Exception: continue
            for cu in chunk_urls(base, txt):
                if cu not in seen_urls: new_urls.add(cu)
        seen_urls |= new_urls
        if not new_urls: break
        print(f"[*] round {rounds}: {len(new_urls)} new chunk urls from manifest")
        before = set(have.values())
        with ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
            list(ex.map(dl_js, new_urls))
        frontier = [p for p in have.values() if p not in before]

    # retry-once any manifest chunk that 404'd (transient failures are real)
    all_manifest = set()
    for lp in list(have.values()):
        try: all_manifest |= chunk_urls(base, open(lp, encoding="utf-8", errors="ignore").read())
        except Exception: pass
    missing = [u for u in all_manifest if u not in have]
    if missing:
        with ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
            list(ex.map(dl_js, missing))
        still = [u for u in all_manifest if u not in have]
        print(f"[*] manifest chunks: {len(all_manifest)} | downloaded {len(have)} | "
              f"unreachable {len(still)} (CSS-only / undeployed)")

    print(f"[+] total JS downloaded: {len(have)}")

    # ---- 新增：sourcemap 与配置材料 ----
    sm_urls, cfg_urls = set(), set()
    for u, lp in list(have.items()):
        try: txt = open(lp, encoding="utf-8", errors="ignore").read()
        except Exception: continue
        for ref in RE_SOURCEMAP.findall(txt):
            abs_u = urllib.parse.urljoin(u, ref)
            if same_origin(base, abs_u):
                sm_urls.add(abs_u)
        for p in re.findall(Q + r'([^"\'`]+\.(?:json|ya?ml))' + Q, txt):
            abs_u = urllib.parse.urljoin(u, p)
            if same_origin(base, abs_u):
                cfg_urls.add(abs_u)

    if sm_urls:
        with ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
            list(ex.map(dl_mat, sm_urls))
        print(f"[+] sourcemaps: {len(sm_urls)} referenced | {len(mats)} materials downloaded")
    if cfg_urls:
        with ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
            list(ex.map(dl_mat, cfg_urls))
        print(f"[+] config candidates: {len(cfg_urls)}")

    # ---- 抽取 ----
    api, other_cfg, routes, methods = set(), set(), set(), set()
    for lp in have.values():
        try: txt = open(lp, encoding="utf-8", errors="ignore").read()
        except Exception: continue
        a, c = extract_endpoints(txt); api |= a; other_cfg |= c
        routes |= extract_routes(txt)
        methods |= set(extract_methods(txt))

    # sourcemap 还原出的未压缩源码再抽一遍——这是命中率最高的一段
    sm_apis, dep_tree = set(), False
    for u, lp in mats.items():
        if not u.endswith(".map"):
            continue
        try: txt = open(lp, encoding="utf-8", errors="ignore").read()
        except Exception: continue
        src, deps = unpack_sourcemap(u, txt)
        dep_tree = dep_tree or deps
        if not src:
            continue
        a, c = extract_endpoints(src); sm_apis |= a; other_cfg |= c
        routes |= extract_routes(src)
        methods |= set(extract_methods(src))

    # 配置材料 → 基址
    bases = set()
    for u, lp in mats.items():
        if u.endswith(".map"):
            continue
        try: txt = open(lp, encoding="utf-8", errors="ignore").read()
        except Exception: continue
        bases |= extract_config_bases(u, txt)

    def dump(name, items):
        path = os.path.join(outdir, name)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write("\n".join(sorted(items)))
        return path

    dump("api_static.txt", api)
    dump("paths_other.txt", other_cfg)
    dump("routes.txt", routes)
    dump("chunkmap.txt", sorted(os.path.basename(u) for u in all_manifest))
    dump("api_methods.txt", {f"{m} {p}" for m, p in methods})
    dump("sourcemaps.txt", sm_urls)
    dump("sm_apis.txt", sm_apis)
    dump("configs.txt", bases)

    if want_coverage:
        # 覆盖报告：只写「这轮到底看到了什么、拿到了什么、没拿到什么」。
        # 关键作用是把「静态收割没覆盖动态面」这件事显式化——否则下游会把
        # 「api_static.txt 只有 12 条」误读成「这个站只有 12 个接口」。
        known = {p for _, p in methods}
        unknown_method = sorted(p for p in api if p not in known)
        only_sm = sorted(set(sm_apis) - set(api))
        covout = {
            "base": base,
            "scripts": {
                "seen": len(cov["js_seen"]),
                "fetched": len(cov["js_ok"]),
                "failed": dict(sorted(cov["js_fail"].items())),
            },
            "materials_failed": dict(sorted(cov["mat_fail"].items())),
            "endpoints": {
                "total": len(api),
                "method_known": len(known),
                "method_unknown": len(unknown_method),
                "method_unknown_sample": unknown_method[:50],
            },
            "endpoints_only_in_sourcemap": only_sm[:200],
            "pages": {
                "seeded": cov["pages_seeded"],
                "note": "只取 robots/sitemap 种子，不执行页面 JS",
            },
            "not_covered": [
                "Tab / 弹窗 / 分页等交互后才发出的请求（需要浏览器，本脚本纯 HTTP）",
                "需要登录态才返回的接口（本脚本不带凭据）",
                "客户端计算后才出现的路径（拼接、加密参数）",
            ],
        }
        covpath = os.path.join(outdir, "coverage.json")
        with open(covpath, "w", encoding="utf-8") as fh:
            json.dump(covout, fh, ensure_ascii=False, indent=2)
        print(f"[+] coverage:  scripts {covout['scripts']['fetched']}/{covout['scripts']['seen']} fetched, "
              f"{len(covout['scripts']['failed'])} failed  -> coverage.json")
        if covout["scripts"]["failed"]:
            print("[!] 未取到的 chunk 会在 coverage.json 的 scripts.failed 里列出原因："
                  "returned_html=SPA 路由回落，http_4xx=服务端不给，empty_or_too_large=超限放弃")

    print(f"[+] api endpoints: {len(api)}  (api_static.txt)")
    print(f"[+] method-known:  {len(methods)} (api_methods.txt)   ← 可直接写进资产图")
    print(f"[+] route paths:   {len(routes)} (routes.txt)")
    print(f"[+] from sourcemap:{len(sm_apis)} (sm_apis.txt)")
    print(f"[+] config bases:  {len(bases)} (configs.txt)" + ("   [dep-tree: yes]" if dep_tree else ""))
    print(f"[+] outdir: {outdir}")
    print("\n[next] reverse the 3 gate facts (see reference.md), fill config.json, "
          "then: node runtime_harvest.js config.json")


if __name__ == "__main__":
    main()

<?php
require __DIR__ . '/inc/auth.php';

$user = portal_current_user();
if ($user === null) {
    if (($_GET['format'] ?? '') === 'json' || str_contains($_SERVER['HTTP_ACCEPT'] ?? '', 'application/json')) {
        portal_json(['ok' => false, 'error' => 'auth_required', 'login' => '/login.php'], 401);
    }
    header('Location: /login.php?e=1', true, 302);
    exit;
}

$sid = $_GET['sid'] ?? '';
$updir = __DIR__ . '/upload';
$files = [];
foreach (is_dir($updir) ? scandir($updir) : [] as $f) {
    if ($f === '.' || $f === '..' || $f === 'index.html') {
        continue;
    }
    $files[] = ['name' => $f, 'size' => filesize($updir . '/' . $f), 'url' => '/upload/' . rawurlencode($f)];
}
usort($files, static fn($a, $b) => strcmp($a['name'], $b['name']));
?>
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>站点管理 - ACME 门户</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f6f8;margin:0;color:#1f2430}
header{background:#1f3a5f;color:#fff;padding:14px 28px;display:flex;justify-content:space-between}
main{max-width:860px;margin:32px auto;background:#fff;border:1px solid #e2e6ee;border-radius:8px;padding:28px}
h1{font-size:20px;margin:0 0 6px}
h2{font-size:15px;margin:28px 0 10px;color:#1f3a5f}
p.hint{color:#8b93a3;font-size:13px;margin:0 0 16px}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #eef1f6}
code{background:#f2f4f8;padding:2px 5px;border-radius:4px}
button{padding:9px 18px;border:0;border-radius:5px;background:#1f3a5f;color:#fff;cursor:pointer}
input[type=file]{font-size:13px}
</style>
</head>
<body>
<header><span>ACME 站点管理</span><span>已登录：<?= htmlspecialchars($user, ENT_QUOTES) ?> &middot; <a href="/logout.php" style="color:#c9d7ea">退出</a></span></header>
<main>
  <h1>部署补丁</h1>
  <p class="hint">上传补丁文件到站点 upload 目录，随后可直接按 URL 访问。支持 .php / .zip / 任意二进制。</p>
  <form method="post" action="/upload.php" enctype="multipart/form-data">
    <input type="hidden" name="sid" value="<?= htmlspecialchars((string) $sid, ENT_QUOTES) ?>">
    <input type="file" name="file" required>
    <button type="submit">上传</button>
  </form>

  <h2>已上传文件</h2>
  <table>
    <tr><th>文件名</th><th>大小</th><th>地址</th></tr>
    <?php if (!$files): ?>
      <tr><td colspan="3" style="color:#8b93a3">暂无</td></tr>
    <?php endif; ?>
    <?php foreach ($files as $f): ?>
      <tr>
        <td><code><?= htmlspecialchars($f['name'], ENT_QUOTES) ?></code></td>
        <td><?= (int) $f['size'] ?> B</td>
        <td><a href="<?= htmlspecialchars($f['url'], ENT_QUOTES) ?>"><?= htmlspecialchars($f['url'], ENT_QUOTES) ?></a></td>
      </tr>
    <?php endforeach; ?>
  </table>
</main>
</body>
</html>

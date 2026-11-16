<?php require __DIR__ . '/inc/auth.php'; $user = portal_current_user(); ?>
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>ACME 门户</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:#f5f6f8;color:#1f2430}
header{background:#1f3a5f;color:#fff;padding:14px 28px;font-weight:600}
main{max-width:760px;margin:48px auto;background:#fff;border:1px solid #e2e6ee;border-radius:8px;padding:32px}
h1{margin:0 0 8px;font-size:24px}
p{line-height:1.7;color:#4a5160}
nav a{margin-right:18px;color:#1f6feb;text-decoration:none}
footer{max-width:760px;margin:24px auto;color:#8b93a3;font-size:12px;text-align:center}
</style>
</head>
<body>
<header>ACME Corp</header>
<main>
  <h1>ACME Corp Portal</h1>
  <p>员工入口 / 客户支持 / 产品文档</p>
  <nav>
    <a href="/docs/">产品文档</a>
    <a href="/login.php">员工登录</a>
    <?php if ($user !== null): ?><a href="/admin.php">站点管理</a><?php endif; ?>
  </nav>
  <!-- TODO(ops): 上线前移除测试备份目录 /backup，别再忘 -->
</main>
<footer>ACME Corp Portal v2.6 &middot; internal build</footer>
</body>
</html>

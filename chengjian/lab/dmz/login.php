<?php
require __DIR__ . '/inc/auth.php';

$wantJSON = (($_GET['format'] ?? '') === 'json')
    || str_contains($_SERVER['HTTP_ACCEPT'] ?? '', 'application/json');

if ($_SERVER['REQUEST_METHOD'] === 'POST') {
    $user = trim((string) ($_POST['username'] ?? ''));
    $pass = (string) ($_POST['password'] ?? '');

    if ($user === '' || $pass === '') {
        if ($wantJSON) {
            portal_json(['ok' => false, 'error' => 'missing_credentials'], 400);
        }
        http_response_code(400);
        echo "missing credentials\n";
        exit;
    }

    if (!hash_equals($CFG['admin_user'], $user) || !hash_equals($CFG['admin_pass'], $pass)) {
        if ($wantJSON) {
            portal_json(['ok' => false, 'error' => 'invalid_credentials'], 401);
        }
        http_response_code(401);
        echo "invalid credentials\n";
        exit;
    }

    $sid = portal_issue($user);
    setcookie('portal_sid', $sid, [
        'expires' => time() + (int) $CFG['session_ttl'],
        'path' => '/',
        'httponly' => true,
        'samesite' => 'Lax',
    ]);
    if ($wantJSON) {
        portal_json(['ok' => true, 'user' => $user, 'sid' => $sid, 'admin' => '/admin.php?sid=' . $sid]);
    }
    header('Location: /admin.php?sid=' . urlencode($sid), true, 302);
    exit;
}

$failed = isset($_GET['e']);
?>
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>员工登录 - ACME 门户</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f5f6f8;margin:0}
form{max-width:340px;margin:96px auto;background:#fff;border:1px solid #e2e6ee;border-radius:8px;padding:28px}
h1{font-size:18px;margin:0 0 20px}
label{display:block;font-size:13px;color:#4a5160;margin:12px 0 4px}
input{width:100%;box-sizing:border-box;padding:9px 10px;border:1px solid #cfd6e3;border-radius:5px;font-size:14px}
button{margin-top:20px;width:100%;padding:10px;border:0;border-radius:5px;background:#1f3a5f;color:#fff;font-size:14px;cursor:pointer}
.err{color:#b42318;font-size:13px;margin-top:12px}
</style>
</head>
<body>
<form method="post" action="/login.php">
  <h1>员工登录</h1>
  <label for="u">用户名</label>
  <input id="u" name="username" autocomplete="username">
  <label for="p">密码</label>
  <input id="p" name="password" type="password" autocomplete="current-password">
  <button type="submit">登录</button>
  <?php if ($failed): ?><div class="err">用户名或密码错误</div><?php endif; ?>
</form>
</body>
</html>

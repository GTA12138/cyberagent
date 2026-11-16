<?php
// 补丁上传：只校验登录态，不校验扩展名——上传即执行（靶场的立足点入口）。
require __DIR__ . '/inc/auth.php';

if (portal_current_user() === null) {
    portal_json(['ok' => false, 'error' => 'auth_required', 'login' => '/login.php'], 401);
}
if ($_SERVER['REQUEST_METHOD'] !== 'POST') {
    portal_json(['ok' => false, 'error' => 'post_required', 'hint' => 'multipart/form-data, 字段名 file'], 405);
}
if (!isset($_FILES['file'])) {
    portal_json(['ok' => false, 'error' => 'no_file', 'hint' => 'multipart/form-data, 字段名 file'], 400);
}

$f = $_FILES['file'];
if (($f['error'] ?? UPLOAD_ERR_NO_FILE) !== UPLOAD_ERR_OK) {
    portal_json(['ok' => false, 'error' => 'upload_error', 'code' => $f['error'] ?? -1], 400);
}

$name = basename(str_replace('\\', '/', (string) $f['name']));
if ($name === '' || $name === '.' || $name === '..') {
    portal_json(['ok' => false, 'error' => 'bad_name'], 400);
}

$dir = __DIR__ . '/upload';
if (!is_dir($dir) && !@mkdir($dir, 0777, true)) {
    portal_json(['ok' => false, 'error' => 'upload_dir_unavailable'], 500);
}

$dest = $dir . '/' . $name;
if (!@move_uploaded_file($f['tmp_name'], $dest)) {
    portal_json(['ok' => false, 'error' => 'move_failed', 'dest' => $dest], 500);
}
@chmod($dest, 0666);

portal_json([
    'ok' => true,
    'name' => $name,
    'size' => filesize($dest),
    'url' => '/upload/' . rawurlencode($name),
]);

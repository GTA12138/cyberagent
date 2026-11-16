<?php
// 前端控制器：只负责「存在的文件交给内置服务器，不存在的给真 404」。
// 没有它的时候 PHP 内置服务器会把所有未知路径回落到 index.php，
// 于是目录扫描全部返回 200/210B，侦察结论完全不可信。
$root = __DIR__;
$path = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH) ?? '/';
$path = urldecode($path);

$target = realpath($root . $path);
if ($target !== false && str_starts_with($target, $root)) {
    if (is_file($target)) {
        return false; // 静态文件或 .php 交给内置服务器处理（.php 会被执行）
    }
    if (is_dir($target)) {
        if (is_file($target . '/index.php') || is_file($target . '/index.html')) {
            return false; // 目录首页
        }
    }
}

http_response_code(404);
header('Content-Type: text/html; charset=utf-8');
echo "<!DOCTYPE html>\n<html><head><title>404 Not Found</title></head><body>\n";
echo "<h1>Not Found</h1>\n<p>The requested URL " . htmlspecialchars($path, ENT_QUOTES) . " was not found on this server.</p>\n";
echo "<hr><address>ACME Portal</address>\n</body></html>\n";

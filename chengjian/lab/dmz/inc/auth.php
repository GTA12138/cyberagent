<?php
// 极简 cookie 会话：无状态签名 token（老站点常见写法，session id 也允许出现在 URL 上）。
$CFG = require __DIR__ . '/config.php';

function portal_token(string $user, int $exp, string $secret): string
{
    $payload = $user . '|' . $exp;
    $mac = hash_hmac('sha256', $payload, $secret);
    return rtrim(strtr(base64_encode($payload), '+/', '-_'), '=') . '.' . $mac;
}

function portal_issue(string $user): string
{
    global $CFG;
    return portal_token($user, time() + (int) $CFG['session_ttl'], $CFG['session_secret']);
}

/** 校验 token；成功返回用户名，失败返回 null。 */
function portal_verify(?string $token): ?string
{
    global $CFG;
    if (!$token || strpos($token, '.') === false) {
        return null;
    }
    [$b64, $mac] = explode('.', $token, 2);
    $payload = base64_decode(strtr($b64, '-_', '+/'), true);
    if ($payload === false || strpos($payload, '|') === false) {
        return null;
    }
    [$user, $exp] = explode('|', $payload, 2);
    if (!hash_equals(hash_hmac('sha256', $payload, $CFG['session_secret']), $mac)) {
        return null;
    }
    if ((int) $exp < time()) {
        return null;
    }
    return $user;
}

/** 取当前登录用户：先看 cookie，再看 ?sid= / POST sid。 */
function portal_current_user(): ?string
{
    if (!empty($_COOKIE['portal_sid'])) {
        $u = portal_verify($_COOKIE['portal_sid']);
        if ($u !== null) {
            return $u;
        }
    }
    $sid = $_GET['sid'] ?? $_POST['sid'] ?? null;
    return is_string($sid) ? portal_verify($sid) : null;
}

function portal_json(array $data, int $code = 200): void
{
    http_response_code($code);
    header('Content-Type: application/json; charset=utf-8');
    echo json_encode($data, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE), "\n";
    exit;
}

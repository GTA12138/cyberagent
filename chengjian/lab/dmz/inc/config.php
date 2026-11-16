<?php
// 门户公共配置（旧站遗留，未迁到环境变量——这正是 /backup 里那份备份泄露的内容）。
return [
    'db_dsn'    => 'mysql://portal_user:Portal_2026!@db.internal:3306/portal',
    'admin_user' => 'portal_user',
    'admin_pass' => 'Portal_2026!',
    'session_secret' => 'acme-portal-2026-legacy-secret',
    'session_ttl'    => 7200,
];

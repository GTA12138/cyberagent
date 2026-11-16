ACME portal — 站点备份说明 2026-10-01
=====================================

站点根目录备份：site-20261001.zip（同目录）
数据库：mysql://portal_user:Portal_2026!@db.internal:3306/portal
后台登录：/login.php    账号 portal_user / Portal_2026!
补丁上传：/upload.php   需登录，落地目录 /upload/，落地后可直接按 URL 访问

运维备注
--------
1. 迁移前确认 /upload/ 可写，否则补丁上传失败。
2. db.internal 只在内网解析，外网访问不到，别在 DMZ 上直连。
3. TODO: 备份目录是临时的，正式上线前整体删掉。

# 来源与许可证

澄鉴（CHENGJIAN）是本地定制项目。上游来源是 [Autumn-27/ARTEX](https://github.com/Autumn-27/ARTEX)，Agent SDK 依赖为 `github.com/Autumn-27/norma`。

本项目保留原 `LICENSE` 及上游权利声明，遵循其中的许可要求。这里的上游名称和链接是来源记录，不是当前产品名称。历史报告、原始证据、备份中的旧名称保留，以免改变原始记录。

当前 Go 模块标识为 `chengjian.local/chengjian`，这是本地模块标识，不是在线仓库地址。本项目尚未配置独立的二进制发布源，更新请使用 `build.ps1`。

数据库已应用迁移文件、旧版密文的密钥派生域和签名测试原文属于不可改写的兼容标识。历史 SQL 保持原校验和，新迁移负责将运行中的数据库对象改为 CHENGJIAN；旧密文可读，新写入使用 gcm2 格式。

# 统一方案最终复核：Web / 身份 / 流量视角

> **【已归档 · 2026-10-23 文档审核】** Web / 身份 / 流量交叉复核记录，七项接口与验收语义已写入方向文件正文，复核已完成。
> 本文是历史记录，不是当前状态：文中写的「未做」可能已完成，文中写的「已完成」也可能已被后续改动推翻。
> 现役进度 → [能力评估与演进方案 §7.0](YMH-能力评估与演进方案.md) ｜ 唯一待办源 → [YMH-待办总表](../2026-10-23/YMH-待办总表.md)

审阅日期：2026-10-21（Asia/Taipei）。审阅对象：同目录《YMH-能力评估与演进方案.md》完整草案（约 393 行的版本）。本记录不修改主报告。已逐项审阅 S0、W1–W4、I1–I4、O1–O2、E1，以及模型、迁移和估算。以下结论仍是设计与静态源码复核，没有运行目标、集成夹具或效果评测。

## 总体结论

**架构方向可接受，建议补齐下列 7 项接口/验收语义后作为实施基线。** 主报告正确保留了 YMH 已有双图、deferred MCP/skill、deterministic stuck 检测、反证 verifier、不可变证据、资产粗覆盖及受管内网原语；没有把这些已有能力误写成全新功能。S0 抓包/读取/证据的接线判断有实际调用链支持，并谨慎标明尚未动态复现。

## 必须明确的 7 项

1. **W1：身份不能仅以 role 或 cookie jar 表达。** AuthContext 应明确 principal、role、tenant、origin、credential_ref、storage_state_ref、auth_state_version；刷新需要按身份的租约/互斥和失效传播。复用现有 `db/credentials.go:25,30,136` 的加密仓。追加两 tenant 同角色、两 worker 同时刷新夹具；否则 IDOR 对照可能实际用了同一个主体，或者刷新轮换互相使会话失效。真实认证工作流应与现有无凭据 mock Recon 分开（`skills/api-recon/SKILL.md:21-22,41`），保留原阶段边界。

2. **W3：来源、可达性、认证和测试结果应分轴建模。** discovered/reachable/authenticated/tested/inconclusive/verified 不能作为一个互斥状态枚举；匿名接口可被验证，登录失败的接口也可有有效侦察结果。至少分 `discovery_source(static/mock/browser/backend)`、`sent_to_backend`、`original_status`、观察 route/auth context、test verdict 和 verification status。mock 触发只增加候选面，不能提升为 reachable/authenticated/tested；静态候选未知 method 保持 unknown。preload 已有 source:'mock'（`skills/api-recon/scripts/preload.js:257,316`），需保留到资产和测试账本；runtime_harvest 会在真实发送前记录并改写状态（`scripts/runtime_harvest.js:153-175`），应显式恢复原始响应引用。

3. **W2/O2：拆分执行结果与结论，并兼容已有 finding 状态。** 当前“工具输出统一 success/negative/auth_expired/…”容易把工具执行成功和目标漏洞阳性放在同一层。建议 `execution_status=completed/error/cancelled/unsupported`，`observation_status=available/auth_expired/transport_error/...`，`assessment=confirmed/refuted/inconclusive`；HTTP 200 或工具 SUCCESS 仅属于观测。W2 实验 verdict 与既有 verifier `verified/false_positive/inconclusive`（`agent/verifier.go:12-16`）通过明确映射或保持独立，不直接改写既有状态机。负向结果需列出前提及覆盖边界。

4. **W4/E1：细化“证据被删”和“已修复”的验收。** 删除可回收的原始 traffic 后，不可变 evidence snapshot 应仍可读取（`evidence/store.go:168-198`）；只有快照缺失、校验失败才属于证据损坏。分别测试这两种情况。宣称“已修复”还须有效身份、相同目标/业务版本和路由、正常对照通过；只有 401、超时或旧状态不存在应为前提失效/无法确认，不是 repaired。ExecutionRecipe 还应区分“还原原实验”与“在新环境重验”，不使用过期秘密硬复刻。

5. **S0/O2/E1：捕获状态是执行回执的一部分。** 除 route/capture policy 外应回报 recorded/metadata_only/passthrough/error/unsupported 和缺失原因。`traffic/traffic.go:720-731` 只有 response 被正常记录，协议错误会触发透传、后续停止录制（`:736-745`）。空 traffic_search 不能证明未发包，更不能证明目标无行为。S0 最小夹具加入协议退化、捕获写入失败及持久化确认；HTTP body 引用不能自动代表 WS/SSE 消息级证据。

6. **S0/迁移：跨任务隔离与显式证据移交需要同时成立。** “跨任务引用被拒绝”应指未授权引用；已有任务来源/移交关系应有明确的只读证据引用或快照复制路径，保留原 task/store/traffic provenance。任务血缘不自动扩大网络 scope，也不自动借用身份或会话。新原始流量引用必须校验 owning task；legacy 全局流量若不能确定任务归属，不得按相同 host/time 猜测补归属。历史读取、导出/恢复与删除路径都需要此规则。

7. **S0/W1/I1/O1：固定快照不能绕过后续授权和目标变化。** 每次网络调用及每次 redirect/cross-origin 子请求重新检查目的 scope、identity origin 和 route applicability；新发现的 URL 只是候选，不自动获得网络访问资格。scope 被撤销时已有 snapshot 不能成为继续执行的许可；应结算原 attempt，再按当前有效 scope 建新 attempt 或阻塞。验收加入跨域重定向、异步浏览器请求和执行中 scope 版本改变，核对每跳路由与原始证据归属。

## 每个工作包的接受结论

| 工作包 | 结论 | 本轮附加条件 |
|---|---|---|
| S0 | 修订后可接受 | 第 5、6、7 项；browser/verifier/session 控制面不得遗漏，历史 store 关闭后仍可寻址 |
| W1 | 修订后可接受 | 第 1、7 项；真实认证与 mock Recon 分阶段 |
| W2 | 修订后可接受 | 第 3 项；baseline/variant 的身份、路由、数据版本及原始证据可追溯 |
| W3 | 修订后可接受 | 第 2 项；保留现有粗资产 coverage，新增 test ledger，勿把候选面误算已测 |
| W4 | 修订后可接受 | 第 3、4 项；复用已有三态 verifier/retester，明确快照完整性与 repaired 判据 |
| I1 | 可接受 | 主方案已表达目标选路、固定版本、控制通道无环；实施纳入第 7 项 |
| I2 | 可接受 | 支持矩阵必须以实际执行器/代理/DNS 能力为准，unsupported 不视作目标失败 |
| I3 | 可接受 | 已明确观察来源、绑定地址、验证时间与逐服务认证；Web 身份与第 1 项共享主体引用 |
| I4 | 可接受 | 已区分局部停止与远端清理确认，移交引用和依赖逆序释放语义足够清楚 |
| O1 | 可接受 | 前提变动定向唤醒、领取时原子检查、重复键和阴性有效期均可验收；纳入第 7 项 |
| O2 | 修订后可接受 | 第 3、5 项；共享 BudgetService 生命周期清楚，保留已有 lazy 机制 |
| E1 | 修订后可接受 | 第 1、2、4、5、7 项增加相应正负和故障夹具；三次重复仅初筛的限定恰当 |

## 模型、接口、迁移和估算

- **模型/接口可接受。** 不强行统一所有协议、不另起事实真值、快照只存引用、外部副作用不承诺 exactly-once 的边界正确。需由第 2、3 项落实状态轴和结果 envelope。LLM 选择通过角色化基准决定，未把特定模型宣传当成工程证据。
- **迁移方案修订后可接受。** 沿用 PG、版本化 payload、明确 legacy namespace、先影子观测再切换和最终单真值均合理；第 6 项补足合法移交和未归属历史流量。
- **32–54 人日可作为受限 MVP 的待校准假设。** 正式承诺前需写清 W1 第一版只支持哪些登录/state 导入与刷新类型、W3 第一版覆盖哪些前端/请求格式，排除“通用 SSO/MFA 自动化、任意前端、全协议、全面 Windows/runner 产品化”的隐含范围。建议在 S0 与最小 E1 完成后按实际失败率重新估算；不要求现在凭静态审阅换成另一个同样无证据的数字。

公开参考主要核对了 Strix 的结构化请求重放/sitemap、machine_observed 与 agent_reported coverage 分离，以及 Shannon 的真实浏览器认证预检/状态保存与 queue 校验错误处理。未运行上游基准，也未把上游 README 的成绩视为独立证明。最终接受不等于已验证效果提升。

## 修订落实确认（2026-10-21）

1. **前述 7 项必改语义均已落实。** 已核对主稿 F09、执行快照与 OperationEnvelope 说明、S0、W1–W4、O2 和迁移条款：主体/tenant/origin 与刷新并发、mock/真实来源及覆盖分轴、执行与判断状态分层、原始流量清理与快照损坏分离、捕获退化回执、显式 grant/移交和 legacy 来源处理、逐跳检查及授权撤销均有明确文字与验收依据。W2 还补充了有状态请求顺序执行和状态重置。
2. **逐包最终结论更新为全部可接受。** S0、W1、W2、W3、W4、I1、I2、I3、I4、O1、O2、E1，及模型、迁移方案均可作为实施设计基线；本轮没有剩余阻塞意见。上文“修订后可接受”的条件现已满足。
3. **估算边界已收敛，可保留为待校准粗估。** MVP 明确排除了通用 SSO/MFA、完整 Windows 原生隧道、新 SSH 驱动和全协议统一代理；影子模式不增发请求、回滚保留回执与证据、store_id 不复用等迁移边界亦已补齐。接受方案不代表已动态验证当前缺口或已证明效果目标，后续仍以 S0/E1 的隔离集成结果为准。

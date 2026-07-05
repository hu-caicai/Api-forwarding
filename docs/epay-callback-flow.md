# 易支付 (Epay) 回调流程说明

## 概述

项目集成了易支付作为在线支付网关，支持两种支付场景：

1. **充值 (TopUp)** — 用户充值账户额度
2. **订阅支付 (Subscription)** — 用户购买订阅套餐

两种场景共享同一个易支付商户配置，但拥有各自独立的回调处理链路。

---

## 配置项

定义在 `setting/operation_setting/payment_setting_old.go`：

| 变量 | 说明 |
|---|---|
| `PayAddress` | 易支付网关地址（如 `https://epay.example.com`） |
| `EpayId` | 商户 ID（PID） |
| `EpayKey` | 商户密钥 |
| `CustomCallbackAddress` | 自定义回调域名（可选，为空则使用 `ServerAddress`） |

回调地址由 `service/epay.go:GetCallbackAddress()` 决定：优先使用 `CustomCallbackAddress`，否则回退到 `system_setting.ServerAddress`。

可用性开关定义在 `controller/payment_webhook_availability.go`：

- `isEpayTopUpEnabled()` — `PayAddress`、`EpayId`、`EpayKey` 均已配置且存在至少一种支付方式
- `isEpayWebhookEnabled()` — 等价于 `isEpayTopUpEnabled()`

---

## 路由注册

定义在 `router/api-router.go`：

### 充值相关路由

| 方法 | 路径 | 处理函数 | 说明 |
|---|---|---|---|
| POST | `/api/user/epay/pay` | `RequestEpay` | 发起充值支付（需用户认证） |
| POST | `/api/user/epay/notify` | `EpayNotify` | 易支付异步通知（免认证，`anonymousRequestBodyLimit` 限流） |
| GET | `/api/user/epay/notify` | `EpayNotify` | 同上（GET 兼容） |

### 订阅相关路由

| 方法 | 路径 | 处理函数 | 说明 |
|---|---|---|---|
| POST | `/api/subscription/epay/pay` | `SubscriptionRequestEpay` | 发起订阅支付（需用户认证） |
| POST | `/api/subscription/epay/notify` | `SubscriptionEpayNotify` | 易支付异步通知（免认证） |
| GET | `/api/subscription/epay/notify` | `SubscriptionEpayNotify` | 同上（GET 兼容） |
| POST | `/api/subscription/epay/return` | `SubscriptionEpayReturn` | 用户支付完成后的浏览器同步跳转 |
| GET | `/api/subscription/epay/return` | `SubscriptionEpayReturn` | 同上（GET 兼容） |

> 充值场景的同步返回由前端在发起支付时通过 `ReturnUrl` 参数直接指定为 `/console/log`，无需服务端处理。

---

## 公用基础设施

### GetEpayClient — `controller/topup.go:136`

```go
func GetEpayClient() *epay.Client {
```

从全局配置读取 `PayAddress`、`EpayId`、`EpayKey`，通过第三方库 `github.com/Calcium-Ion/go-epay/epay` 创建易支付客户端。任一配置为空时返回 nil。

### 订单锁 — `controller/topup.go:276`

```go
func LockOrder(tradeNo string)
func UnlockOrder(tradeNo string)
```

使用引用计数的 `sync.Mutex` 映射实现订单级别互斥锁，防止同一订单的并发回调处理。锁机制细节：

- `LockOrder`：按 `tradeNo` 获取或创建互斥锁，引用计数 +1，加锁
- `UnlockOrder`：解锁，引用计数 -1，归零时从 map 中删除

---

## 场景一：充值回调流程

### 1. 发起充值 — `RequestEpay` (`controller/topup.go:190`)

```
用户 → POST /api/user/epay/pay → RequestEpay
```

完整流程：

1. **参数校验**：解析 `EpayRequest{Amount, PaymentMethod}`，校验金额 >= 最低充值额
2. **金额计算**：`getPayMoney(amount, group)` 根据用户分组比率、单价、折扣计算实际支付金额
3. **支付方式校验**：`ContainsPayMethod()` 检查支付方式是否在已配置列表中
4. **构建回调地址**：`notifyUrl = CallbackAddress + "/api/user/epay/notify"`，`returnUrl` 固定为 `/console/log`
5. **生成订单号**：`USR{userId}NO{随机6位}{时间戳}`
6. **创建易支付订单**：调用 `client.Purchase()` 跳转到易支付网关
7. **落地订单**：创建 `model.TopUp` 记录（状态 `pending`）
8. **响应**：返回易支付网关 URL 和表单参数给前端

### 2. 异步通知 — `EpayNotify` (`controller/topup.go:316`)

```
易支付 → POST/GET /api/user/epay/notify → EpayNotify
```

完整流程：

```
┌─────────────────────────────────────────────────────────────────┐
│ EpayNotify                                                     │
├─────────────────────────────────────────────────────────────────┤
│ 1. webhook 开关检查 ── 未启用则返回 "fail"                     │
│ 2. 解析请求参数（POST form 或 GET query）                     │
│ 3. 参数为空 → 返回 "fail"                                      │
│ 4. GetEpayClient() → 获取失败返回 "fail"                       │
│ 5. client.Verify(params) 验签                                  │
│    ├─ 验签失败 → 返回 "fail"                                   │
│    └─ 验签成功 → 返回 "success" 给易支付                       │
│ 6. 检查 TradeStatus == StatusTradeSuccess                      │
│    ├─ 否 → 日志记录后退出                                      │
│    └─ 是 → 进入订单处理                                        │
│ 7. LockOrder(tradeNo) 加锁                                     │
│ 8. 查询订单 GetTopUpByTradeNo()                                │
│    ├─ 不存在 → 日志警告后退出                                  │
│    ├─ PaymentProvider 不匹配 → 日志警告后退出                  │
│    └─ 存在且 Status == pending → 处理                          │
│ 9. 更新订单状态为 success                                      │
│10. 计算额度 IncreaseUserQuota()                                │
│11. RecordTopupLog() 记录充值日志                               │
│12. UnlockOrder(tradeNo) 解锁                                   │
└─────────────────────────────────────────────────────────────────┘
```

关键处理逻辑：

- **返回 "success" 的时机**：验签通过即立即返回，**不等待**订单处理完成。这是为了防止易支付因 HTTP 超时而重复回调
- **订单幂等性**：`topUp.Status` 判断，只处理 `pending` 状态的订单
- **Provider 校验**：确保订单的 `PaymentProvider` 为 `Epay`，防止跨网关回调攻击
- **实际支付方式更新**：如果回调中的支付类型与订单记录不同，会更新订单的 `PaymentMethod`

额度计算：

```go
quotaToAdd = amount * QuotaPerUnit
model.IncreaseUserQuota(userId, quotaToAdd, true)
```

### 异常场景处理

| 场景 | 行为 |
|---|---|
| Webhook 未启用 | 日志记录后返回 "fail" |
| 参数为空 | 返回 "fail" |
| Client 未初始化 | 返回 "fail" |
| 验签失败 | 返回 "fail", 记录 warn 日志 |
| 订单不存在 | 记录 warn 日志，不继续处理 |
| 支付网关不匹配 | 记录 warn 日志，不继续处理 |
| 订单非 pending 状态 | 跳过（已处理或已过期） |
| 更新数据库失败 | 记录 error 日志，不继续处理（人工介入补单） |

### 3. 管理员补单 — `AdminCompleteTopUp` (`controller/topup.go:501`)

```
管理员 → POST /api/（后端接口） → AdminCompleteTopUp
```

当异步通知因网络等原因未送达时，管理员可手动补单：调用 `model.ManualCompleteTopUp()` 完成订单状态更新和额度发放。

---

## 场景二：订阅支付回调流程

### 1. 发起订阅支付 — `SubscriptionRequestEpay` (`controller/subscription_payment_epay.go:25`)

```
用户 → POST /api/subscription/epay/pay → SubscriptionRequestEpay
```

流程：

1. **合规检查**：`requirePaymentCompliance()`
2. **参数校验**：解析 `SubscriptionEpayPayRequest{PlanId, PaymentMethod}`
3. **套餐校验**：检查套餐是否存在、已启用、金额 >= 0.01
4. **支付方式校验**：`ContainsPayMethod()`
5. **购买上限校验**：如果套餐设置了 `MaxPurchasePerUser`，检查用户已购买数量
6. **构建回调地址**：
   - `notifyUrl = CallbackAddress + "/api/subscription/epay/notify"`
   - `returnUrl = CallbackAddress + "/api/subscription/epay/return"`
7. **生成订单号**：`SUBUSR{userId}NO{随机6位}{时间戳}`
8. **创建订单**：`model.SubscriptionOrder`（状态 `pending`）
9. **拉起支付**：调用 `client.Purchase()`
10. **响应**：返回易支付网关 URL 和表单参数

### 2. 异步通知 — `SubscriptionEpayNotify` (`controller/subscription_payment_epay.go:122`)

```
易支付 → POST/GET /api/subscription/epay/notify → SubscriptionEpayNotify
```

```
┌─────────────────────────────────────────────────────────────────┐
│ SubscriptionEpayNotify                                         │
├─────────────────────────────────────────────────────────────────┤
│ 1. 解析请求参数（POST form 或 GET query）                     │
│ 2. 参数为空 → 返回 "fail"                                      │
│ 3. GetEpayClient() → 获取失败返回 "fail"                       │
│ 4. client.Verify(params) 验签                                  │
│    ├─ 失败 → 返回 "fail"                                       │
│    └─ 成功 → 继续                                              │
│ 5. TradeStatus == StatusTradeSuccess?                           │
│    ├─ 否 → 返回 "fail"                                         │
│    └─ 是 → 继续                                                │
│ 6. LockOrder(tradeNo) 加锁                                     │
│ 7. model.CompleteSubscriptionOrder()                           │
│    ├─ 失败 → 返回 "fail"                                       │
│    └─ 成功 → 返回 "success"                                    │
│ 8. UnlockOrder(tradeNo) 解锁                                   │
└─────────────────────────────────────────────────────────────────┘
```

订阅订单完成 — `model.CompleteSubscriptionOrder()` (`model/subscription.go:553`)

1. 在数据库事务中执行：
   - `SELECT ... FOR UPDATE` 锁定订单行
   - 验证 `PaymentProvider` 匹配
   - 状态必须是 `pending`；已完成的直接返回 nil（幂等）
   - 获取套餐信息并调用 `CreateUserSubscriptionFromPlanTx()` 创建用户订阅记录
   - 调用 `upsertSubscriptionTopUpTx()` 创建/更新充值记录
   - 更新订单状态为 `success`，记录 `CompleteTime` 和 `ProviderPayload`
2. 事务提交后：
   - 如果套餐配置了升级分组（`UpgradeGroup`），更新用户分组缓存
   - 记录操作日志

### 3. 同步跳转 — `SubscriptionEpayReturn` (`controller/subscription_payment_epay.go:177`)

```
用户浏览器 → GET/POST /api/subscription/epay/return → SubscriptionEpayReturn
```

```
┌─────────────────────────────────────────────────────────────────┐
│ SubscriptionEpayReturn                                         │
├─────────────────────────────────────────────────────────────────┤
│ 1. 解析请求参数                                                │
│ 2. 参数为空 → redirect /console/topup?pay=fail                 │
│ 3. GetEpayClient()                                             │
│ 4. client.Verify(params) 验签                                  │
│    ├─ 失败 → redirect /console/topup?pay=fail                  │
│    └─ 成功 → 继续                                              │
│ 5. TradeStatus == StatusTradeSuccess?                           │
│    ├─ 是 → LockOrder → CompleteSubscriptionOrder               │
│    │    ├─ 成功 → redirect /console/topup?pay=success          │
│    │    └─ 失败 → redirect /console/topup?pay=fail             │
│    └─ 否 → redirect /console/topup?pay=pending                 │
└─────────────────────────────────────────────────────────────────┘
```

同步跳转的特点：

- 与异步通知功能**重叠**：也执行 `CompleteSubscriptionOrder()`，但通过**重定向而非直接响应**
- 用作**兜底**：如果异步通知因网络问题未送达，用户浏览器跳回时完成订单
- **幂等**：`CompleteSubscriptionOrder()` 内部已做幂等处理
- 交易状态非成功时跳转到 `pay=pending`，避免让用户误以为支付失败

---

## 提升可观测性

- 发起支付成功时：`LogInfo("易支付 充值订单创建成功")`
- 收到回调时：`LogInfo("易支付 webhook 收到请求")` + `LogInfo("易支付 webhook 验签成功")`
- 验签失败时：`LogWarn("易支付 webhook 验签失败")`
- 订单不存在时：`LogWarn("易支付 回调订单不存在")`
- 充值成功时：`LogInfo("易支付 充值成功")`
- 更新数据库失败时：`LogError("易支付 更新充值订单失败")`

---

## 关键设计要点

1. **提前响应 vs. 延迟处理**：充值的 `EpayNotify` 在验签通过后**立即返回 "success"** 给易支付，然后才处理订单业务逻辑。这是为了避免易支付网关因等待超时而重复发送回调。订阅的 `SubscriptionEpayNotify` 则是在完成订单后才返回（订单处理在事务中，持续时间较短）。

2. **跨网关防护**：通过 `PaymentProvider` 校验防止恶意构造其他支付网关的回调请求。订阅场景在数据库事务中通过 `SELECT ... FOR UPDATE` 加行锁。

3. **订单级别互斥锁**：使用引用计数的 `sync.Map` 实现的 `LockOrder/UnlockOrder`，确保同一订单不会被并发处理两次。

4. **幂等性保证**：`CompleteSubscriptionOrder` 和 `EpayNotify` 都只处理 `pending` 状态的订单。已完成的订单直接返回成功，避免重复处理。

5. **同步跳转兜底**：订阅场景同时实现了异步通知和同步跳转两条路径，任一路径先到达都能完成订单，后到达的因状态已变更会幂等跳过。

6. **支付方式更新**：如果易支付网关回调中的实际支付方式与订单创建时不同（例如用户在易支付页面切换了支付方式），系统会更新订单记录以反映实际支付方式。

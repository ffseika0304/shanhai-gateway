# api.seika.ltd 接口契约

> 版本：0.3.0 ｜ 基础地址：`https://api.seika.ltd`  
> **小程序开发照此文档，不用等后端先跑。**

---

## 一、认证接口

### 1.1 小程序登录

```
POST /api/auth/wechat/mini
Content-Type: application/json

{ "code": "wx.login 返回的 code" }
```

成功返回：

```json
{ "token": "session_token 字符串", "user_id": 1, "expires_at": 1789661787 }
```

失败：`{"error":"wechat auth failed"}` 或 `{"error":"wechat unreachable"}`

---

### 1.2 PC 扫码：生成 ticket

```
POST /api/auth/qr/generate
返回：{ "ticket": "随机字符串", "expires_in": 300 }
```

### 1.3 PC 扫码：小程序确认

```
POST /api/auth/qr/confirm
Content-Type: application/json

{ "ticket": "从二维码获取", "code": "wx.login 返回的 code" }
```

成功：`{ "status": "ok" }`  
失败：`{"error":"ticket expired"}` / `{"error":"wechat auth failed"}`

### 1.4 PC 扫码：轮询状态（WebSocket 的 fallback）

```
GET /api/auth/qr/status?ticket=xxx
```

返回：`{ "status": "confirmed", "token": "session_token", "user_id": 1 }`

### 1.5 WebSocket（PC 端实时推送）

```
wss://api.seika.ltd/ws/qr?ticket=xxx
```

连接后等待服务端推送 session token。超时 5 分钟。

---

## 二、用户中心（需鉴权）

所有 `/api/user/*` 接口需要传 `X-Session-Token` header。

### 2.1 获取当前用户

```
GET /api/user/me
X-Session-Token: <token>

返回：{ "user_id": 1, "nickname": "", "avatar": "", "quota": 1000, "used": 0, "remaining": 1000 }
```

### 2.2 生成 API Key

```
POST /api/user/keys/generate
X-Session-Token: <token>
Content-Type: application/json
{ "name": "my-key" }

返回：{ "key": "sk-user-xxx", "name": "my-key" }
```

⚠️ 完整 key 只在创建时返回一次，后续查询只能看到脱敏后的。

### 2.3 列出 API Key

```
GET /api/user/keys
X-Session-Token: <token>

返回：[{ "id": 1, "key_masked": "sk-user-cc9c...a66a", "name": "test", "enabled": true, "created_at": 123 }]
```

### 2.4 删除 API Key

```
DELETE /api/user/keys/{id}
X-Session-Token: <token>

返回：{ "status": "ok" }
```

---

## 三、模型调用（用 API Key）

```
POST /v1/chat/completions
Authorization: Bearer sk-user-xxx
Content-Type: application/json

{
  "model": "deepseek-v4-flash",
  "messages": [{ "role": "user", "content": "你好" }],
  "max_tokens": 100
}
```

---

## 四、数据面

```
GET  /v1/models          — 模型列表
GET  /health             — 健康检查
GET  /api/metrics        — 运行指标
```

---

## 五、认证流程图

```
小程序用户                          PC 浏览器
    |                                  |
    |  wx.login() → code               |
    |  POST /api/auth/wechat/mini      |
    |  ← token                         |
    |  存 token，进入首页              |
    |                                  |
    |  扫码（扫 PC 生成的二维码）      |
    |  POST /api/auth/qr/confirm       |
    |  { ticket, code }                |
    |  → status: ok                    |
    |                                  |  ← WebSocket 收到 token
    |                                  |  → 进入 dashboard
```

---

## 六、环境变量

| 变量                   | 说明                      |
| -------------------- | ----------------------- |
| `WECHAT_MINI_APPID`  | 小程序 AppID               |
| `WECHAT_MINI_SECRET` | 小程序 AppSecret           |
| `DEV_MODE`           | `1` 启用 dev-login，`0` 关闭 |

---

## 七、注意事项

- `session_token` 有效期 7 天
- QR ticket 有效期 5 分钟
- 每个用户最多 5 个 API Key
- WebSocket 无响应时 5 分钟自动超时
- 额度用完后 API Key 返回 403

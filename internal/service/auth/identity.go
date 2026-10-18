package auth

import "time"

// 幂等常量：defaultSessionTTL 出自 issueSession 的 `ttl <= 0 → 7 天`。
const defaultSessionTTL = 7 * 24 * time.Hour

// Identity 是「认证通过」这一事实的载体。
// 每一种登录方式最终都要产出它，再交给 Service.IssueSession 换成 session
// token —— 那是**唯一的汇合点**，所以新增登录方式不需要碰别的文件。
type Identity struct {
	Subject  string        // 外部唯一标识（用户名 / openid / "admin"）
	Nickname string        //
	Avatar   string        //
	IsAdmin  bool          // 标记管理员（登录方式自己判定，网关不认密码）
	TTL      time.Duration // 会话有效期；<=0 时用默认 7 天
}

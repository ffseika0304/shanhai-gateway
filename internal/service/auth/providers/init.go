// Package providers 是 auth 内置登录方式的实现集。
//
// 新增一种登录方式：在下面写一个实现 + 在 init() 里 auth.RegisterLogin 一行，
// 其余代码（/v1 鉴权、用户中心、额度扣减、看板）完全不动。
//
// 接入方负责 blank import（或显式 import）本包以触发注册 —— 接线还没拆到
// main 之前，这里先保持包独立。
package providers

import (
	"os"

	"shanhaigateway/internal/service/auth"
)

func init() {
	// 注册表存工厂：ADMIN_PASSWORD / DEV_MODE 在接线（RegisterEnabledLogins）
	// 时才读（缘由见 auth.ProviderFactory 注释）。
	auth.RegisterLogin(AdminName, func(svc *auth.Service) auth.LoginProvider {
		return AdminProvider{svc: svc, pw: os.Getenv("ADMIN_PASSWORD")}
	})
	auth.RegisterLogin(DevName, func(svc *auth.Service) auth.LoginProvider {
		return DevProvider{svc: svc, enabled: os.Getenv("DEV_MODE") == "1"}
	})
}

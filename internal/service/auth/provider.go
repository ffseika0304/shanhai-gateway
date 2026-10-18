package auth

import (
	"log"
	"net/http"
	"sort"
)

// ── 登录方式抽象层 ────────────────────────────────────────────────
//
// 登录方式（管理员密码 / … / 未来的 OIDC / 短信 / 扫码）全部收敛到下面这个
// interface。**新增一种登录方式 = 写一个实现 + 一行注册，不动其他任何代码。**
//
// 为什么让 Provider 自己 Register 路由，而不是由框架统一挂载：
//   - 单步登录（POST 直接换 token）→ 注册一个 handler 就行
//   - 多步登录（先拿 ticket、外部确认、再换 token）→ 需要多条路由
//   强制统一会为了迁就最简单的形态而把复杂形态憋成特例，所以这里放权给 Provider。
//
// 无论哪种形态，最终都调用 Service.IssueSession(Identity{...}) —— 那是所有
// 登录方式唯一的汇合点。

// LoginProvider 一种登录方式。
type LoginProvider interface {
	// Name 标识，用于注册表和启动日志。
	Name() string
	// Enabled 是否启用（读配置/env 决定）。false 则整条方式不挂路由。
	Enabled() bool
	// Register 把这条登录方式需要的 HTTP 路由挂到 mux 上。
	Register(mux *http.ServeMux)
}

// ProviderFactory 由 Service 造出 provider 实例。
// 注册表里存工厂而不是实例：provider 要用 IssueSession，而 Service（连带它
// 的 store）只在接线（RegisterEnabledLogins）时才存在；同时 env（ADMIN_PASSWORD
// / DEV_MODE）在工厂里读，t.Setenv 才能测到。旧代码在 init() 里构造好实例，
// 生产行为一致（都在启动期），只是把这一步从 import 时移到接线时。
type ProviderFactory func(svc *Service) LoginProvider

// loginProviders 全局注册表。写入只发生在各 provider 包的 init()（import 期，
// 单线程序）、读取只发生在启动接线，无并发争用 —— 靠这层约定而没加锁，
// 注释在这里钉住。
var loginProviders = map[string]ProviderFactory{}

// RegisterLogin 注册一种登录方式，在 provider 包的 init() 里调用。
func RegisterLogin(name string, f ProviderFactory) { loginProviders[name] = f }

// RegisterEnabledLogins 把所有启用的登录方式挂到 mux，并打启动日志。
// 原 registerEnabledLogins，只是从自由函数变成 Service 的方法（要传 store）。
func (s *Service) RegisterEnabledLogins(mux *http.ServeMux) {
	for _, name := range sortedProviderNames() {
		p := loginProviders[name](s)
		if !p.Enabled() {
			log.Printf("[auth] 登录方式 %q 未启用", name)
			continue
		}
		p.Register(mux)
		log.Printf("[auth] 登录方式 %q 已启用", name)
	}
}

// sortedProviderNames 让启动日志顺序稳定（map 遍历是随机的）。
// 原来是手写插入排序，借用标准库即可 —— 行为一致。
func sortedProviderNames() []string {
	names := make([]string, 0, len(loginProviders))
	for n := range loginProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

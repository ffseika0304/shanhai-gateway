// Package web —— 两个前端（go:embed 的唯一宿主）。
//
// 为什么开成独立小包而不是塞进 internal/api 或 cmd/gateway：//go:embed 的
// 路径必须落在包目录之内，塞不进 ../web —— 想嵌就得把 html 挪进那些目录，
// 而前端就该待在 web/。这个包**只有 embed 变量、没有任何代码**，
// console/（公版用户中心）与 dashboard/（运维看板）由接线层 fs.Sub 后交给
// internal/api 挂载。改名/开源换品牌只动 html 与 identity.go。
package web

import "embed"

// Console 公版用户中心（原根目录 static/index.html，重命名保真搬入 → /console/）。
//
//go:embed console/index.html
var Console embed.FS

// Dashboard 运维看板（自原生产调度网关的前端拷入 → /）。
//
//go:embed dashboard/index.html
var Dashboard embed.FS

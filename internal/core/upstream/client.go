package upstream

import (
	"net/http"
	"time"
)

// Client 返回该后端的持久化 HTTP 客户端（懒加载，之后一直复用）。
//
// ⚠️ 连接池参数照搬，不要改：
//   - MaxIdleConns 50 / MaxIdleConnsPerHost 10：复用长连接，别每次重连。
//   - IdleConnTimeout 90s：配合上游的 keep-alive 窗口（通常 60~120s）。
//   - Timeout 默认 300s：长流式响应会占着连接，不能给太短。
//
// 搬入时的核对结论：源实现（seika_llm_router main.go getClient）**没有**设置
// ForceAttemptHTTP2，Go 的 DefaultTransport 因此保留 HTTP/2。这里照原样保留，
// 没有补写任何 Transport 字段 —— 补一个就等于改连接行为，不是搬运。
func (b *Backend) Client() *http.Client {
	if b.client != nil {
		return b.client
	}
	timeout := time.Duration(b.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	b.client = &http.Client{Timeout: timeout, Transport: &http.Transport{
		MaxIdleConns: 50, MaxIdleConnsPerHost: 10,
		IdleConnTimeout: 90 * time.Second,
	}}
	return b.client
}

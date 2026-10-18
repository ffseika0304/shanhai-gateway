package routing

import (
	"fmt"
	"sync"
	"testing"

	"shanhai-gateway/internal/core/upstream"
)

// ── AtomicRouter：原子换装的崩溃保护 ─────────────────────────────
//
// 合同就一条：**读者在任何时刻 Load 到的都是完整 Router**。
// 若有人（或未来重构）在半建状态把指针切过去，读者立刻就能查到一个
// 「少后端 / 空列表」的路由器 —— 下面的断言会当场炸出来，而不是等到
// 生产上某个请求 503 才暴露。

// atomicRouterWith 用 N 个占位后端建一个 Router（Name = b0..b{N-1}）。
func atomicRouterWith(t *testing.T, n int) *Router {
	t.Helper()
	bs := make([]*upstream.Backend, n)
	for i := range bs {
		b := upstream.New(fmt.Sprintf("b%d", i), "", "http://127.0.0.1:9/v1", nil)
		bs[i] = b
	}
	return New(bs)
}

// TestAtomicRouterConcurrentSwap 并发读写：写方反复换装（换前对象完整建好），
// 读方持续 Load 并断言当前 Router 永远包含**全部**已知后端。
// 在 -race 下跑：任意半建指针切换都会被读者立刻发现。
func TestAtomicRouterConcurrentSwap(t *testing.T) {
	const n = 3
	ar := NewAtomicRouter(atomicRouterWith(t, n))

	stop := make(chan struct{})
	errs := make(chan string, 16)
	wgWait := &sync.WaitGroup{}

	// 读方
	for g := 0; g < 4; g++ {
		wgWait.Add(1)
		go func() {
			defer wgWait.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rt := ar.Load()
				for i := 0; i < n; i++ {
					name := fmt.Sprintf("b%d", i)
					if rt.ByName(name) == nil {
						errs <- fmt.Sprintf("读者看到不完整 Router：缺 %s（指针被半建对象抢跑）", name)
						return
					}
				}
			}
		}()
	}

	// 写方：反复建新 Router（完整建好后）再换装
	wgWrite := &sync.WaitGroup{}
	for g := 0; g < 2; g++ {
		wgWrite.Add(1)
		go func() {
			defer wgWrite.Done()
			for i := 0; i < 50; i++ {
				next := atomicRouterWith(t, n) // 完整建好
				ar.Store(next)                 // 才允许换装
			}
		}()
	}
	wgWrite.Wait()
	close(stop)
	wgWait.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
}

// TestAtomicRouterSwapReplacesThat 替换语义：Swap 之后 Load 必须立刻看到新
// Router（不读到旧对象），且旧对象仍在跑的请求不受影响（这里用旧引用可继续
// 用代表「持有旧指针的请求照常完成」）。
func TestAtomicRouterSwapReplaces(t *testing.T) {
	old := atomicRouterWith(t, 1)
	ar := NewAtomicRouter(old)

	next := atomicRouterWith(t, 2)
	ar.Store(next)

	got := ar.Load()
	if got == old {
		t.Fatal("Store 后 Load 仍返回旧 Router")
	}
	if got.ByName("b1") == nil {
		t.Fatal("新 Router 应包含 b1")
	}
	// 旧对象依旧完整可用（相当于换装瞬间还在飞的请求）
	if old.ByName("b0") == nil {
		t.Fatal("旧 Router 应仍可用")
	}
}

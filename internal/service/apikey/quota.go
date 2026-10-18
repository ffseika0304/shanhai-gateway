package apikey

// quotaExceeded 统一额度判定：quota>=0 且 used>=quota 即用完。
//
// 这是两条分支（sk-user 的 DB 额度 / keys.json 的内存额度）原来各自内联的
// 同一行判断（公版 main.go：`quota >= 0 && used >= quota` 与
// `k.Quota >= 0 && k.Used >= int64(k.Quota)`）。抽成一处防两分支日后漂移；
// quota<0 = 不限额的原语义照搬。
func quotaExceeded(quota, used int64) bool {
	return quota >= 0 && used >= quota
}

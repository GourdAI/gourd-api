package admin

// defaultSubscriptionTestFloat64 返回 v 的指针，用于构造
// service.DefaultSubscriptionSetting.TotalLimitUSD / service.UserSubscription.TotalLimitUSD
// 以及 BulkAssignSubscriptionRequest.TotalLimitUSD 等 *float64 字段。
//
// 为什么单独定义而不复用 channel_handler_test.go 里的 float64Ptr：
// 那个文件带 `//go:build unit` 标签，默认构建（go vet / 无 tag 的 go test -c）下不可见。
func defaultSubscriptionTestFloat64(v float64) *float64 {
	return &v
}

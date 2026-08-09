package service

// nvidiaPrewarmBlockChecker 报告账号当前是否处于运行时封锁状态。
// NVIDIAInferencePrewarmer 用它跳过封锁中账号的预热请求，避免在 429 封锁期
// 继续向上游发送预热流量（方案 A）。nil 注入时预热器行为与旧版一致。
//
// OpenAIGatewayService 实现该接口（委托 openaiAccountRuntimeBlockUntil 判定）。
type nvidiaPrewarmBlockChecker interface {
	IsNvidiaPrewarmBlocked(accountID int64) bool
}

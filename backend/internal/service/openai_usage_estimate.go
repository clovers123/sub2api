package service

// openAIUsageIsEmpty 判断 usage 是否完全没有可计费 token。
// 只要任一 token 维度 > 0 即视为非空（含 ImageInputTokens 等补充计费
// 字段）：D 方案的估算兜底只应在「根本拿不到 usage」时触发，不能覆盖
// 上游已返回的 cache/image 部分计费。
func openAIUsageIsEmpty(u OpenAIUsage) bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.ImageInputTokens == 0 &&
		u.ImageOutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 &&
		u.CacheReadInputTokens == 0
}

// estimateOpenAIUsageFallback 在 NVIDIA 免费 API 未返回 usage 时用字节数
// 估算 token 用量兜底，避免下游零计费。
//
//   - InputTokens = inputBytes / 4（字节→token 近似，避免 tiktoken 依赖）
//   - OutputTokens = outputBytes / 4
//
// 只填 InputTokens/OutputTokens 两个字段；低位截断（不足 4 字节的余数
// 丢弃，不会负溢出）。其余字段保持零值，避免伪造 cache/image 计费。
func estimateOpenAIUsageFallback(inputBytes int, outputBytes int) OpenAIUsage {
	if inputBytes < 0 {
		inputBytes = 0
	}
	if outputBytes < 0 {
		outputBytes = 0
	}
	return OpenAIUsage{
		InputTokens:  inputBytes / 4,
		OutputTokens: outputBytes / 4,
	}
}
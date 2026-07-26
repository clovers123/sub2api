package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}

// NVIDIAConnectionPrewarmUpstream NVIDIA 上游连接预热能力接口。
//
// 独立于 HTTPUpstream 定义（可选能力接口），避免所有 HTTPUpstream 测试桩
// 都被迫实现预热方法；repository 的 httpUpstreamService 同时实现两个接口。
type NVIDIAConnectionPrewarmUpstream interface {
	// PrewarmNVIDIAConnection 对指定代理（空字符串表示直连）预热 NVIDIA 上游连接：
	// 复用 NVIDIA 共享连接池的客户端条目，发送一次不携带凭据的探测请求，
	// 完成 TCP+TLS+H2 建连。任何 HTTP 状态码（含 401）均视为成功，
	// 仅传输层错误返回 error。
	PrewarmNVIDIAConnection(ctx context.Context, proxyURL string) error
}

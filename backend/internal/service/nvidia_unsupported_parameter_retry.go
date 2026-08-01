package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// nvidiaUnsupportedParameterRE 解析 NVIDIA 400 错误体中 "Unsupported parameter(s): xxx"
// 的字段名。NVIDIA 错误消息格式通常为:
//
//	"Unsupported parameter(s): 'previous_response_id'"
//	"Unsupported parameters: 'prompt_cache_key', 'tool_choice'"
//
// 正则同时支持单数 / 复数 / 引号可选；捕获组匹配字段名。
var nvidiaUnsupportedParameterRE = regexp.MustCompile(`(?i)unsupported\s+parameter(?:s)?\s*[:=]\s*(.+)`)

// nvidia Quoted field 相当于 JSON 解析 quoted 列表；字段名被 NVIDIA 用单引号或带
// 反引号包裹；我们显式匹配这些包裹符并提取名字。
var nvidiaQuotedFieldRE = regexp.MustCompile(`[` + "`" + `"'"]([a-zA-Z_][a-zA-Z0-9_\.]*)[` + "`" + `"'"]`)

// tryNVIDIARetryOnUnsupportedParameter 在 NVIDIA 上游返回 400 且 body 含 "Unsupported
// parameter(s): xxx" 时，解析出被拒绝的字段名列表，从请求体剥离后允许 caller 重试
// 一次。返回 (newBody, true, nil) 表示可重试；返回 (_, false, nil) 表示不是 NVIDIA
// unsupported parameter 错误或不该重试。
//
// 设计要点:
//   - 自动发现: 不预设 NVIDIA 不支持字段清单，而是从错误响应中实时解析，避免
//     NVIDIA 后续新增不支持字段时清单失配
//   - 一次性重试: 仅重试一次，避免循环；caller 负责计数
//   - 不 fail-over: 剥离字段后重试同一账号，因为这是参数问题而非账号健康问题
//   - 严格字段名校验: 仅接受字母+数字+下划线的字段名，防止正则误匹配注入
func tryNVIDIARetryOnUnsupportedParameter(responseBody []byte, requestBody []byte) (newBody []byte, retryable bool, err error) {
	if len(responseBody) == 0 || len(requestBody) == 0 {
		return requestBody, false, nil
	}

	match := nvidiaUnsupportedParameterRE.FindSubmatch(responseBody)
	if match == nil {
		return requestBody, false, nil
	}

	// match[1] 是字段名段，可能是单个或用逗号分隔的多个。
	rawFields := match[1]
	fieldNames := nvidiaQuotedFieldRE.FindAllSubmatch(rawFields, -1)
	if len(fieldNames) == 0 {
		// fallback: 整段 strip 后按逗号切分 + trim 引号
		for _, part := range strings.Split(string(rawFields), ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, "`'\"")
			if part == "" || !isValidNVIDIAParamName(part) {
				continue
			}
			fieldNames = append(fieldNames, [][]byte{[]byte(part)})
		}
	}

	if len(fieldNames) == 0 {
		return requestBody, false, nil
	}

	stripped := requestBody
	strippedAny := false
	for _, fn := range fieldNames {
		name := string(fn[1])
		if !isValidNVIDIAParamName(name) {
			continue
		}
		updated, sErr := sjson.DeleteBytes(stripped, name)
		if sErr != nil {
			continue
		}
		if updated != nil {
			stripped = updated
			strippedAny = true
			logger.L().Info("nvidia unsupported parameter retry: stripped field",
				zap.String("field", name),
			)
		}
	}

	if !strippedAny {
		return requestBody, false, nil
	}
	return stripped, true, nil
}

// isValidNVIDIAParamName 严格校验 NVIDIA 不支持参数的字段名:
// 仅允许字母开头 + 字母数字下划线 + 可选点路径（嵌套字段如 messages.0.role）。
// 防止正则误匹配注入到 sjson.DeleteBytes 的路径。
func isValidNVIDIAParamName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		isFirst := i == 0
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case !isFirst && (r >= '0' && r <= '9'):
		case !isFirst && r == '.':
		default:
			return false
		}
	}
	return true
}

// detectNVIDIAUnsupportedParameterError 检测 upstream 400 响应是否为 NVIDIA
// "Unsupported parameter" 错误。返回被拒绝的字段名列表（按出现顺序）。
// 当响应不是 NVIDIA 400 unsupported parameter 时返回空 slice。
func detectNVIDIAUnsupportedParameterError(statusCode int, responseBody []byte) []string {
	if statusCode != http.StatusBadRequest || len(responseBody) == 0 {
		return nil
	}
	match := nvidiaUnsupportedParameterRE.FindSubmatch(responseBody)
	if match == nil {
		return nil
	}
	rawFields := match[1]
	quoted := nvidiaQuotedFieldRE.FindAllSubmatch(rawFields, -1)
	if len(quoted) == 0 {
		for _, part := range strings.Split(string(rawFields), ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, "`'\"")
			if part != "" && isValidNVIDIAParamName(part) {
				quoted = append(quoted, [][]byte{[]byte(part)})
			}
		}
	}
	result := make([]string, 0, len(quoted))
	for _, q := range quoted {
		name := string(q[1])
		if isValidNVIDIAParamName(name) {
			result = append(result, name)
		}
	}
	return result
}

// logNVIDIAUnsupportedParameterRetry 在重试成功或失败时统一记录日志。
// 通过统一入口让运维在 grep `nvidia_unsupported_parameter_retry` 时
// 能看到所有 NVIDIA 400 重试事件（不论成败）。
func logNVIDIAUnsupportedParameterRetry(ctx context.Context, account *Account, fields []string, retryResult string) {
	logger.L().Info("nvidia_unsupported_parameter_retry",
		zap.Int64("account_id", accountIDForLog(account)),
		zap.Strings("stripped_fields", fields),
		zap.String("retry_result", retryResult),
	)
}

// formatNVIDIAUnsupportedFields 用于错误信息和日志中转出可读字段名列表。
func formatNVIDIAUnsupportedFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	b, _ := json.Marshal(fields)
	return string(b)
}

// nvidiaUnsupportedParameterError 包装 NVIDIA 上游 400 unsupported parameter 错误，
// 供 caller 在 fail-over 或返回客户端时给出更明确的错误消息。
type nvidiaUnsupportedParameterError struct {
	StatusCode int
	Fields     []string
	RawBody    []byte
}

func (e *nvidiaUnsupportedParameterError) Error() string {
	if len(e.Fields) > 0 {
		return fmt.Sprintf("nvidia unsupported parameter: %s", strings.Join(e.Fields, ", "))
	}
	return "nvidia unsupported parameter"
}

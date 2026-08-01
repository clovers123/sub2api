//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsValidNVIDIAParamName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "simple_field", in: "previous_response_id", want: true},
		{name: "underscore_start_ok", in: "_private", want: true},
		{name: "dot_path_nested", in: "messages.0.role", want: true},
		{name: "digits_after_first_ok", in: "tool_choice_2", want: true},
		{name: "digit_first_rejected", in: "1bad", want: false},
		{name: "dash_rejected", in: "tool-choice", want: false},
		{name: "space_rejected", in: "tool choice", want: false},
		{name: "bracket_rejected", in: "messages[0]", want: false},
		{name: "exactly_128_letters_accepted", in: makeName(128), want: true},
		{name: "129_letters_rejected", in: makeName(129), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isValidNVIDIAParamName(tt.in))
		})
	}
}

func makeName(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}

func TestDetectNVIDIAUnsupportedParameterError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       []string
	}{
		{name: "empty_body", statusCode: http.StatusBadRequest, body: nil, want: nil},
		{name: "wrong_status_ignored", statusCode: http.StatusInternalServerError, body: []byte("Unsupported parameter(s): 'foo'"), want: nil},
		{name: "single_singular_form", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameter: 'previous_response_id'"),
			want: []string{"previous_response_id"}},
		{name: "plural_form", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameters: 'prompt_cache_key', 'tool_choice'"),
			want: []string{"prompt_cache_key", "tool_choice"}},
		{name: "case_insensitive", statusCode: http.StatusBadRequest,
			body: []byte("unsupported PARAMETERS: 'a'"),
			want: []string{"a"}},
		{name: "backtick_quoted", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameters: `previous_response_id`"),
			want: []string{"previous_response_id"}},
		{name: "double_quote", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameters: \"tool_choice\""),
			want: []string{"tool_choice"}},
		{name: "literal_parens_s_form", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameter(s): 'previous_response_id'"),
			want: []string{"previous_response_id"}},
		{name: "literal_parens_s_multiple", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameter(s): 'prompt_cache_key', 'tool_choice'"),
			want: []string{"prompt_cache_key", "tool_choice"}},
		{name: "invalid_field_dropped", statusCode: http.StatusBadRequest,
			body: []byte("Unsupported parameters: 'good', 'valid_one'"),
			want: []string{"good", "valid_one"}},
		{name: "no_keyword_returns_nil", statusCode: http.StatusBadRequest,
			body: []byte("Some other NVIDIA error"),
			want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectNVIDIAUnsupportedParameterError(tt.statusCode, tt.body)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("fallback_no_quotes_returns_fields", func(t *testing.T) {
		got := detectNVIDIAUnsupportedParameterError(http.StatusBadRequest, []byte("Unsupported parameters: foo, bar"))
		require.Equal(t, []string{"foo", "bar"}, got, "bug fixed: fallback path now safely returns parsed fields")
	})
}

func TestTryNVIDIARetryOnUnsupportedParameter(t *testing.T) {
	tests := []struct {
		name         string
		responseBody []byte
		requestBody  []byte
		wantRetry    bool
		wantBodyKW   []string // must contain these keys in resulting body
		wantStripKW  []string // must NOT contain these keys
	}{
		{
			name:         "nil_inputs_no_retry",
			responseBody: nil,
			requestBody:  nil,
			wantRetry:    false,
		},
		{
			name:         "not_unsupported_param_error",
			responseBody: []byte("Internal Server Error"),
			requestBody:  []byte(`{"prompt_cache_key":"x"}`),
			wantRetry:    false,
		},
		{
			name:         "single_field_stripped",
			responseBody: []byte("Unsupported parameters: 'previous_response_id'"),
			requestBody: []byte(`{"messages":"hi","previous_response_id":"abc"}`),
			wantRetry:    true,
			wantBodyKW:   []string{"messages"},
			wantStripKW:  []string{"previous_response_id"},
		},
		{
			name:         "multiple_fields_stripped",
			responseBody: []byte("Unsupported parameters: 'prompt_cache_key', 'tool_choice'"),
			requestBody: []byte(`{"model":"m","prompt_cache_key":"k","tool_choice":"auto"}`),
			wantRetry:    true,
			wantBodyKW:   []string{"model"},
			wantStripKW:  []string{"prompt_cache_key", "tool_choice"},
		},
		{
			name:         "field_not_present_in_body_no_retry",
			responseBody: []byte("Unsupported parameters: 'previous_response_id'"),
			requestBody:  []byte(`{"model":"m"}`),
			wantRetry:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newBody, retry, err := tryNVIDIARetryOnUnsupportedParameter(tt.responseBody, tt.requestBody)
			require.NoError(t, err)
			require.Equal(t, tt.wantRetry, retry, "retry flag mismatch")
			if !retry {
				require.Equal(t, tt.requestBody, newBody, "on no-retry, body must be returned unchanged")
				return
			}
			for _, kw := range tt.wantBodyKW {
				require.Contains(t, string(newBody), kw)
			}
			for _, kw := range tt.wantStripKW {
				require.NotContains(t, string(newBody), kw)
			}
		})
	}
}

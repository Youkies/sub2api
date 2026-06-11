package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	// creditsExhaustedKey 是 model_rate_limits 中标记积分耗尽的特殊 key。
	// 与普通模型限流完全同构：通过 SetModelRateLimit / isRateLimitActiveForKey 读写。
	creditsExhaustedKey      = "AICredits"
	creditsExhaustedDuration = 2 * time.Hour
)

type antigravity429Category string

const (
	antigravity429Unknown        antigravity429Category = "unknown"
	antigravity429RateLimited    antigravity429Category = "rate_limited"
	antigravity429QuotaExhausted antigravity429Category = "quota_exhausted"
	// antigravity429AmbiguousTransient 裸 gRPC RESOURCE_EXHAUSTED，无结构化 ErrorInfo。
	// 通常是节点/上游级别压力，非单一账号信号，使用较短冷却时间。
	antigravity429AmbiguousTransient antigravity429Category = "ambiguous_transient"
)

// antigravityAmbiguousTransientDuration 裸 gRPC 无结构信息时的冷却时间（5 分钟 + 抖动，上限 15 分钟）。
const (
	antigravityAmbiguousTransientBase = 5 * time.Minute
	antigravityAmbiguousTransientCap  = 15 * time.Minute
)

var (
	antigravityQuotaExhaustedKeywords = []string{
		"quota_exhausted",
		"quota exhausted",
		"exhausted your capacity",
	}

	// creditsExhaustedKeywords 仅包含明确指向 AI Credits 余额不足的关键词。
	// 不包含裸 "resource has been exhausted"，避免将节点瞬时压力误判为积分耗尽。
	creditsExhaustedKeywords = []string{
		"google_one_ai",
		"insufficient credit",
		"insufficient credits",
		"not enough credit",
		"not enough credits",
		"credit exhausted",
		"credits exhausted",
		"credit balance",
		"minimumcreditamountforusage",
		"minimum credit amount for usage",
		"minimum credit",
	}
)

// isCreditsExhausted 检查账号的 AICredits 限流 key 是否生效（积分是否耗尽）。
func (a *Account) isCreditsExhausted() bool {
	if a == nil {
		return false
	}
	return a.isRateLimitActiveForKey(creditsExhaustedKey)
}

// setCreditsExhausted 标记账号积分耗尽：写入 model_rate_limits["AICredits"] + 更新缓存。
// 冷却时间加 ±25% 抖动，防止多账号在同一时刻同步恢复。
func (s *AntigravityGatewayService) setCreditsExhausted(ctx context.Context, account *Account) {
	if account == nil || account.ID == 0 {
		return
	}
	cooldown := antigravityJitterDuration(creditsExhaustedDuration, antigravityRateLimitJitterPct)
	resetAt := time.Now().Add(cooldown)
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, creditsExhaustedKey, resetAt); err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "set credits exhausted failed: account=%d err=%v", account.ID, err)
		return
	}
	s.updateAccountModelRateLimitInCache(ctx, account, creditsExhaustedKey, resetAt)
	logger.LegacyPrintf("service.antigravity_gateway", "credits_exhausted_marked account=%d reset_at=%s cooldown=%v",
		account.ID, resetAt.UTC().Format(time.RFC3339), cooldown.Truncate(time.Second))
}

// clearCreditsExhausted 清除账号的 AICredits 限流 key。
func (s *AntigravityGatewayService) clearCreditsExhausted(ctx context.Context, account *Account) {
	if account == nil || account.ID == 0 || account.Extra == nil {
		return
	}
	rawLimits, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return
	}
	if _, exists := rawLimits[creditsExhaustedKey]; !exists {
		return
	}
	delete(rawLimits, creditsExhaustedKey)
	account.Extra[modelRateLimitsKey] = rawLimits
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		modelRateLimitsKey: rawLimits,
	}); err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "clear credits exhausted failed: account=%d err=%v", account.ID, err)
	}
}

// classifyAntigravity429 将 Antigravity 的 429 响应归类。
// 分类优先级（由高到低）：
//  1. 结构化 ErrorInfo reason == QUOTA_EXHAUSTED                 → QuotaExhausted
//  2. 结构化 RetryInfo + reason == RATE_LIMIT_EXCEEDED + delay > 60s → QuotaExhausted
//  3. 结构化 RetryInfo + reason == RATE_LIMIT_EXCEEDED + delay ≤ 60s → RateLimited
//  4. 关键词匹配 quota_exhausted / exhausted your capacity        → QuotaExhausted
//  5. 裸 gRPC RESOURCE_EXHAUSTED（无结构化 details）              → AmbiguousTransient
//  6. 其他                                                        → Unknown
func classifyAntigravity429(body []byte) antigravity429Category {
	if len(body) == 0 {
		return antigravity429Unknown
	}

	// 优先使用结构化 ErrorInfo/RetryInfo 分类
	if info := parseAntigravitySmartRetryInfo(body); info != nil && !info.IsModelCapacityExhausted {
		if info.RetryDelay >= antigravityRateLimitThreshold {
			return antigravity429QuotaExhausted
		}
		return antigravity429RateLimited
	}

	// 检查 QUOTA_EXHAUSTED reason（结构化但 parseAntigravitySmartRetryInfo 未覆盖的格式）
	if isQuotaExhaustedReason(body) {
		return antigravity429QuotaExhausted
	}

	// 关键词匹配
	lowerBody := strings.ToLower(string(body))
	for _, keyword := range antigravityQuotaExhaustedKeywords {
		if strings.Contains(lowerBody, keyword) {
			return antigravity429QuotaExhausted
		}
	}

	// 裸 gRPC 默认 RESOURCE_EXHAUSTED（无结构化 details，非单账号信号）
	if strings.Contains(lowerBody, "resource has been exhausted") &&
		!strings.Contains(lowerBody, "capacity on this model") {
		return antigravity429AmbiguousTransient
	}

	return antigravity429Unknown
}

// isQuotaExhaustedReason 检查 JSON 中是否有 reason == "QUOTA_EXHAUSTED" 的 ErrorInfo。
func isQuotaExhaustedReason(body []byte) bool {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		return false
	}
	details, ok := errObj["details"].([]any)
	if !ok {
		return false
	}
	for _, d := range details {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if reason, ok := dm["reason"].(string); ok && reason == "QUOTA_EXHAUSTED" {
			return true
		}
	}
	return false
}

// injectEnabledCreditTypes 在已序列化的 v1internal JSON body 中注入 AI Credits 类型。
func injectEnabledCreditTypes(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	payload["enabledCreditTypes"] = []string{"GOOGLE_ONE_AI"}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return result
}

// resolveCreditsOveragesModelKey 解析当前请求对应的 overages 状态模型 key。
func resolveCreditsOveragesModelKey(ctx context.Context, account *Account, upstreamModelName, requestedModel string) string {
	modelKey := strings.TrimSpace(upstreamModelName)
	if modelKey != "" {
		return modelKey
	}
	if account == nil {
		return ""
	}
	modelKey = resolveFinalAntigravityModelKey(ctx, account, requestedModel)
	if strings.TrimSpace(modelKey) != "" {
		return modelKey
	}
	return resolveAntigravityModelKey(requestedModel)
}

// shouldMarkCreditsExhausted 判断一次 credits 请求失败是否应标记为 credits 耗尽。
func shouldMarkCreditsExhausted(resp *http.Response, respBody []byte, reqErr error) bool {
	if reqErr != nil || resp == nil {
		return false
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
		return false
	}
	// 仅在响应包含明确的积分相关关键词时才标记耗尽。
	// 裸 "Resource has been exhausted" 不在判断范围内：该响应是节点级压力信号，
	// 与账号积分余额无关，误标记会导致后续积分路径被跳过。
	if info := parseAntigravitySmartRetryInfo(respBody); info != nil {
		return false
	}
	bodyLower := strings.ToLower(string(respBody))
	for _, keyword := range creditsExhaustedKeywords {
		if strings.Contains(bodyLower, keyword) {
			return true
		}
	}
	return false
}

type creditsOveragesRetryResult struct {
	handled bool
	resp    *http.Response
}

// attemptCreditsOveragesRetry 在确认免费配额耗尽后，尝试注入 AI Credits 继续请求。
func (s *AntigravityGatewayService) attemptCreditsOveragesRetry(
	p antigravityRetryLoopParams,
	baseURL string,
	modelName string,
	waitDuration time.Duration,
	originalStatusCode int,
	respBody []byte,
) *creditsOveragesRetryResult {
	creditsBody := injectEnabledCreditTypes(p.body)
	if creditsBody == nil {
		return &creditsOveragesRetryResult{handled: false}
	}
	modelKey := resolveCreditsOveragesModelKey(p.ctx, p.account, modelName, p.requestedModel)
	logger.LegacyPrintf("service.antigravity_gateway", "%s status=429 credit_overages_retry model=%s account=%d (injecting enabledCreditTypes)",
		p.prefix, modelKey, p.account.ID)

	creditsReq, err := antigravity.NewAPIRequestWithURL(p.ctx, baseURL, p.action, p.accessToken, creditsBody)
	if err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "%s credit_overages_failed model=%s account=%d build_request_err=%v",
			p.prefix, modelKey, p.account.ID, err)
		return &creditsOveragesRetryResult{handled: true}
	}

	creditsResp, err := p.httpUpstream.Do(creditsReq, p.proxyURL, p.account.ID, p.account.Concurrency)
	if err == nil && creditsResp != nil && creditsResp.StatusCode < 400 {
		s.clearCreditsExhausted(p.ctx, p.account)
		logger.LegacyPrintf("service.antigravity_gateway", "%s status=%d credit_overages_success model=%s account=%d",
			p.prefix, creditsResp.StatusCode, modelKey, p.account.ID)
		return &creditsOveragesRetryResult{handled: true, resp: creditsResp}
	}

	s.handleCreditsRetryFailure(p.ctx, p.prefix, modelKey, p.account, creditsResp, err)
	return &creditsOveragesRetryResult{handled: true}
}

func (s *AntigravityGatewayService) handleCreditsRetryFailure(
	ctx context.Context,
	prefix string,
	modelKey string,
	account *Account,
	creditsResp *http.Response,
	reqErr error,
) {
	var creditsRespBody []byte
	creditsStatusCode := 0
	if creditsResp != nil {
		creditsStatusCode = creditsResp.StatusCode
		if creditsResp.Body != nil {
			creditsRespBody, _ = io.ReadAll(io.LimitReader(creditsResp.Body, 64<<10))
			_ = creditsResp.Body.Close()
		}
	}

	if shouldMarkCreditsExhausted(creditsResp, creditsRespBody, reqErr) && account != nil {
		s.setCreditsExhausted(ctx, account)
		logger.LegacyPrintf("service.antigravity_gateway", "%s credit_overages_failed model=%s account=%d marked_exhausted=true status=%d body=%s",
			prefix, modelKey, account.ID, creditsStatusCode, truncateForLog(creditsRespBody, 200))
		return
	}
	if account != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "%s credit_overages_failed model=%s account=%d marked_exhausted=false status=%d err=%v body=%s",
			prefix, modelKey, account.ID, creditsStatusCode, reqErr, truncateForLog(creditsRespBody, 200))
	}
}

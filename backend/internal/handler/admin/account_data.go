package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"log/slog"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	dataType       = "sub2api-data"
	legacyDataType = "sub2api-bundle"
	dataVersion    = 1
	dataPageCap    = 1000
)

type DataPayload struct {
	Type       string        `json:"type,omitempty"`
	Version    int           `json:"version,omitempty"`
	ExportedAt string        `json:"exported_at"`
	Proxies    []DataProxy   `json:"proxies"`
	Groups     []DataGroup   `json:"groups"`
	Accounts   []DataAccount `json:"accounts"`
}

type DataProxy struct {
	ProxyKey string `json:"proxy_key"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Status   string `json:"status"`
}

type DataGroup struct {
	Name             string  `json:"name"`
	Description      string  `json:"description,omitempty"`
	Platform         string  `json:"platform"`
	RateMultiplier   float64 `json:"rate_multiplier"`
	IsExclusive      bool    `json:"is_exclusive"`
	Status           string  `json:"status"`
	SubscriptionType string  `json:"subscription_type,omitempty"`
}

// DataAccount 是管理员显式备份导出使用的账号结构，故意不走 dto.Account 的脱敏路径，
// Credentials 原文返回。这是"管理员备份"这一显式行为的一部分；如未来需要导出脱敏版本，
// 应新增独立结构而非修改这里。
type DataAccount struct {
	Name               string         `json:"name"`
	Notes              *string        `json:"notes,omitempty"`
	Platform           string         `json:"platform"`
	Type               string         `json:"type"`
	Credentials        map[string]any `json:"credentials"`
	Extra              map[string]any `json:"extra,omitempty"`
	ProxyKey           *string        `json:"proxy_key,omitempty"`
	GroupNames         []string       `json:"group_names,omitempty"`
	Concurrency        int            `json:"concurrency"`
	Priority           int            `json:"priority"`
	RateMultiplier     *float64       `json:"rate_multiplier,omitempty"`
	ExpiresAt          *int64         `json:"expires_at,omitempty"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired,omitempty"`
}

type DataImportRequest struct {
	Data                 DataPayload `json:"data"`
	SkipDefaultGroupBind *bool       `json:"skip_default_group_bind"`
	OverwriteExisting    *bool       `json:"overwrite_existing"`
}

type DataImportResult struct {
	ProxyCreated   int               `json:"proxy_created"`
	ProxyReused    int               `json:"proxy_reused"`
	ProxyFailed    int               `json:"proxy_failed"`
	GroupCreated   int               `json:"group_created"`
	GroupReused    int               `json:"group_reused"`
	GroupFailed    int               `json:"group_failed"`
	AccountCreated int               `json:"account_created"`
	AccountUpdated int               `json:"account_updated"`
	AccountFailed  int               `json:"account_failed"`
	Errors         []DataImportError `json:"errors,omitempty"`
}

type DataImportError struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	ProxyKey string `json:"proxy_key,omitempty"`
	Message  string `json:"message"`
}

func buildProxyKey(protocol, host string, port int, username, password string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", strings.TrimSpace(protocol), strings.TrimSpace(host), port, strings.TrimSpace(username), strings.TrimSpace(password))
}

func buildGroupKey(platform, name string) string {
	return strings.ToLower(strings.TrimSpace(platform)) + "|" + strings.TrimSpace(name)
}

func (h *AccountHandler) ExportData(c *gin.Context) {
	ctx := c.Request.Context()

	selectedIDs, err := parseAccountIDs(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	accounts, err := h.resolveExportAccounts(ctx, selectedIDs, c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	includeProxies, err := parseIncludeProxies(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	includeGroups, err := parseIncludeGroups(c)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var proxies []service.Proxy
	if includeProxies {
		proxies, err = h.resolveExportProxies(ctx, accounts)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else {
		proxies = []service.Proxy{}
	}

	proxyKeyByID := make(map[int64]string, len(proxies))
	dataProxies := make([]DataProxy, 0, len(proxies))
	for i := range proxies {
		p := proxies[i]
		key := buildProxyKey(p.Protocol, p.Host, p.Port, p.Username, p.Password)
		proxyKeyByID[p.ID] = key
		dataProxies = append(dataProxies, DataProxy{
			ProxyKey: key,
			Name:     p.Name,
			Protocol: p.Protocol,
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			Password: p.Password,
			Status:   p.Status,
		})
	}

	groupByID, dataGroups := resolveDataGroups(accounts, includeGroups)

	dataAccounts := make([]DataAccount, 0, len(accounts))
	for i := range accounts {
		acc := accounts[i]
		var proxyKey *string
		if acc.ProxyID != nil {
			if key, ok := proxyKeyByID[*acc.ProxyID]; ok {
				proxyKey = &key
			}
		}
		var expiresAt *int64
		if acc.ExpiresAt != nil {
			v := acc.ExpiresAt.Unix()
			expiresAt = &v
		}
		groupNames := resolveAccountGroupNames(acc, groupByID, includeGroups)
		dataAccounts = append(dataAccounts, DataAccount{
			Name:               acc.Name,
			Notes:              acc.Notes,
			Platform:           acc.Platform,
			Type:               acc.Type,
			Credentials:        acc.Credentials,
			Extra:              acc.Extra,
			ProxyKey:           proxyKey,
			GroupNames:         groupNames,
			Concurrency:        acc.Concurrency,
			Priority:           acc.Priority,
			RateMultiplier:     acc.RateMultiplier,
			ExpiresAt:          expiresAt,
			AutoPauseOnExpired: &acc.AutoPauseOnExpired,
		})
	}

	payload := DataPayload{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Proxies:    dataProxies,
		Groups:     dataGroups,
		Accounts:   dataAccounts,
	}

	response.Success(c, payload)
}

func (h *AccountHandler) ImportData(c *gin.Context) {
	var req DataImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if err := validateDataHeader(req.Data); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	executeAdminIdempotentJSON(c, "admin.accounts.import_data", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.importData(ctx, req)
	})
}

func (h *AccountHandler) importData(ctx context.Context, req DataImportRequest) (DataImportResult, error) {
	skipDefaultGroupBind := true
	if req.SkipDefaultGroupBind != nil {
		skipDefaultGroupBind = *req.SkipDefaultGroupBind
	}
	overwriteExisting := true
	if req.OverwriteExisting != nil {
		overwriteExisting = *req.OverwriteExisting
	}

	dataPayload := req.Data
	result := DataImportResult{}
	existingAccounts, err := h.listAccountsFiltered(ctx, "", "", "", "", 0, "", "created_at", "desc")
	if err != nil {
		return result, err
	}

	existingProxies, err := h.listAllProxies(ctx)
	if err != nil {
		return result, err
	}

	proxyKeyToID := make(map[string]int64, len(existingProxies))
	for i := range existingProxies {
		p := existingProxies[i]
		key := buildProxyKey(p.Protocol, p.Host, p.Port, p.Username, p.Password)
		proxyKeyToID[key] = p.ID
	}

	for i := range dataPayload.Proxies {
		item := dataPayload.Proxies[i]
		key := item.ProxyKey
		if key == "" {
			key = buildProxyKey(item.Protocol, item.Host, item.Port, item.Username, item.Password)
		}
		if err := validateDataProxy(item); err != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:     "proxy",
				Name:     item.Name,
				ProxyKey: key,
				Message:  err.Error(),
			})
			continue
		}
		normalizedStatus := normalizeProxyStatus(item.Status)
		if existingID, ok := proxyKeyToID[key]; ok {
			proxyKeyToID[key] = existingID
			result.ProxyReused++
			if normalizedStatus != "" {
				if proxy, getErr := h.adminService.GetProxy(ctx, existingID); getErr == nil && proxy != nil && proxy.Status != normalizedStatus {
					_, _ = h.adminService.UpdateProxy(ctx, existingID, &service.UpdateProxyInput{
						Status: normalizedStatus,
					})
				}
			}
			continue
		}

		created, createErr := h.adminService.CreateProxy(ctx, &service.CreateProxyInput{
			Name:     defaultProxyName(item.Name),
			Protocol: item.Protocol,
			Host:     item.Host,
			Port:     item.Port,
			Username: item.Username,
			Password: item.Password,
		})
		if createErr != nil {
			result.ProxyFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:     "proxy",
				Name:     item.Name,
				ProxyKey: key,
				Message:  createErr.Error(),
			})
			continue
		}
		proxyKeyToID[key] = created.ID
		result.ProxyCreated++

		if normalizedStatus != "" && normalizedStatus != created.Status {
			_, _ = h.adminService.UpdateProxy(ctx, created.ID, &service.UpdateProxyInput{
				Status: normalizedStatus,
			})
		}
	}

	// 收集需要异步设置隐私的 Antigravity OAuth 账号
	existingGroups, err := h.listAllGroups(ctx)
	if err != nil {
		return result, err
	}

	groupKeyToID := make(map[string]int64, len(existingGroups)+len(dataPayload.Groups))
	groupNameToID := make(map[string]int64, len(existingGroups)+len(dataPayload.Groups))
	for i := range existingGroups {
		g := existingGroups[i]
		groupKeyToID[buildGroupKey(g.Platform, g.Name)] = g.ID
		if _, exists := groupNameToID[g.Name]; !exists {
			groupNameToID[g.Name] = g.ID
		}
	}

	for i := range dataPayload.Groups {
		item := dataPayload.Groups[i]
		if err := validateDataGroup(item); err != nil {
			result.GroupFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "group",
				Name:    item.Name,
				Message: err.Error(),
			})
			continue
		}
		key := buildGroupKey(item.Platform, item.Name)
		if existingID, ok := groupKeyToID[key]; ok {
			groupKeyToID[key] = existingID
			groupNameToID[item.Name] = existingID
			result.GroupReused++
			continue
		}

		created, createErr := h.adminService.CreateGroup(ctx, &service.CreateGroupInput{
			Name:             item.Name,
			Description:      item.Description,
			Platform:         item.Platform,
			RateMultiplier:   item.RateMultiplier,
			IsExclusive:      item.IsExclusive,
			SubscriptionType: item.SubscriptionType,
		})
		if createErr != nil {
			result.GroupFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "group",
				Name:    item.Name,
				Message: createErr.Error(),
			})
			continue
		}
		groupKeyToID[key] = created.ID
		groupNameToID[item.Name] = created.ID
		result.GroupCreated++
	}

	var privacyAccounts []*service.Account

	for i := range dataPayload.Accounts {
		item := dataPayload.Accounts[i]
		if err := validateDataAccount(item); err != nil {
			result.AccountFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "account",
				Name:    item.Name,
				Message: err.Error(),
			})
			continue
		}

		var proxyID *int64
		if item.ProxyKey != nil && *item.ProxyKey != "" {
			if id, ok := proxyKeyToID[*item.ProxyKey]; ok {
				proxyID = &id
			} else {
				result.AccountFailed++
				result.Errors = append(result.Errors, DataImportError{
					Kind:     "account",
					Name:     item.Name,
					ProxyKey: *item.ProxyKey,
					Message:  "proxy_key not found",
				})
				continue
			}
		}

		groupIDs, groupResolveErr := resolveImportGroupIDs(item.GroupNames, item.Platform, groupKeyToID, groupNameToID)
		if groupResolveErr != nil {
			result.AccountFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "account",
				Name:    item.Name,
				Message: groupResolveErr.Error(),
			})
			continue
		}

		enrichCredentialsFromIDToken(&item)

		accountInput := &service.CreateAccountInput{
			Name:                  item.Name,
			Notes:                 item.Notes,
			Platform:              item.Platform,
			Type:                  item.Type,
			Credentials:           item.Credentials,
			Extra:                 item.Extra,
			ProxyID:               proxyID,
			Concurrency:           item.Concurrency,
			Priority:              item.Priority,
			RateMultiplier:        item.RateMultiplier,
			GroupIDs:              groupIDs,
			ExpiresAt:             item.ExpiresAt,
			AutoPauseOnExpired:    item.AutoPauseOnExpired,
			SkipDefaultGroupBind:  skipDefaultGroupBind,
			SkipMixedChannelCheck: true,
		}

		var created *service.Account
		if overwriteExisting {
			if existing := findExistingDataAccount(item, existingAccounts); existing != nil {
				groupIDsCopy := append([]int64(nil), groupIDs...)
				updateProxyID := proxyID
				if updateProxyID == nil {
					clearProxyID := int64(0)
					updateProxyID = &clearProxyID
				}
				created, err = h.adminService.UpdateAccount(ctx, existing.ID, &service.UpdateAccountInput{
					Name:                  item.Name,
					Notes:                 item.Notes,
					Type:                  item.Type,
					Credentials:           item.Credentials,
					Extra:                 item.Extra,
					ProxyID:               updateProxyID,
					Concurrency:           &item.Concurrency,
					Priority:              &item.Priority,
					RateMultiplier:        item.RateMultiplier,
					GroupIDs:              &groupIDsCopy,
					ExpiresAt:             item.ExpiresAt,
					AutoPauseOnExpired:    item.AutoPauseOnExpired,
					SkipMixedChannelCheck: true,
				})
				if err == nil {
					result.AccountUpdated++
				}
			}
		}
		if created == nil && err == nil {
			created, err = h.adminService.CreateAccount(ctx, accountInput)
			if err == nil {
				result.AccountCreated++
			}
		}
		if err != nil {
			result.AccountFailed++
			result.Errors = append(result.Errors, DataImportError{
				Kind:    "account",
				Name:    item.Name,
				Message: err.Error(),
			})
			continue
		}
		// 收集 Antigravity OAuth 账号，稍后异步设置隐私
		if created.Platform == service.PlatformAntigravity && created.Type == service.AccountTypeOAuth {
			privacyAccounts = append(privacyAccounts, created)
		}
	}

	// 异步设置 Antigravity 隐私，避免大量导入时阻塞请求
	if len(privacyAccounts) > 0 {
		adminSvc := h.adminService
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("import_antigravity_privacy_panic", "recover", r)
				}
			}()
			bgCtx := context.Background()
			for _, acc := range privacyAccounts {
				adminSvc.ForceAntigravityPrivacy(bgCtx, acc)
			}
			slog.Info("import_antigravity_privacy_done", "count", len(privacyAccounts))
		}()
	}

	return result, nil
}

func (h *AccountHandler) listAllProxies(ctx context.Context) ([]service.Proxy, error) {
	page := 1
	pageSize := dataPageCap
	var out []service.Proxy
	for {
		items, total, err := h.adminService.ListProxies(ctx, page, pageSize, "", "", "", "created_at", "desc")
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) >= int(total) || len(items) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (h *AccountHandler) listAllGroups(ctx context.Context) ([]service.Group, error) {
	page := 1
	pageSize := dataPageCap
	var out []service.Group
	for {
		items, total, err := h.adminService.ListGroups(ctx, page, pageSize, "", "", "", nil, "created_at", "desc")
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) >= int(total) || len(items) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (h *AccountHandler) listAccountsFiltered(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode, sortBy, sortOrder string) ([]service.Account, error) {
	page := 1
	pageSize := dataPageCap
	var out []service.Account
	for {
		items, total, err := h.adminService.ListAccounts(ctx, page, pageSize, platform, accountType, status, search, groupID, privacyMode, sortBy, sortOrder)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(out) >= int(total) || len(items) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (h *AccountHandler) resolveExportAccounts(ctx context.Context, ids []int64, c *gin.Context) ([]service.Account, error) {
	if len(ids) > 0 {
		accounts, err := h.adminService.GetAccountsByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		out := make([]service.Account, 0, len(accounts))
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			out = append(out, *acc)
		}
		return out, nil
	}

	platform := c.Query("platform")
	accountType := c.Query("type")
	status := c.Query("status")
	privacyMode := strings.TrimSpace(c.Query("privacy_mode"))
	search := strings.TrimSpace(c.Query("search"))
	sortBy := c.DefaultQuery("sort_by", "name")
	sortOrder := c.DefaultQuery("sort_order", "asc")
	if len(search) > 100 {
		search = search[:100]
	}

	groupID := int64(0)
	if groupIDStr := c.Query("group"); groupIDStr != "" {
		if groupIDStr == accountListGroupUngroupedQueryValue {
			groupID = service.AccountListGroupUngrouped
		} else {
			parsedGroupID, parseErr := strconv.ParseInt(groupIDStr, 10, 64)
			if parseErr != nil || parsedGroupID <= 0 {
				return nil, infraerrors.BadRequest("INVALID_GROUP_FILTER", "invalid group filter")
			}
			groupID = parsedGroupID
		}
	}

	return h.listAccountsFiltered(ctx, platform, accountType, status, search, groupID, privacyMode, sortBy, sortOrder)
}

func (h *AccountHandler) resolveExportProxies(ctx context.Context, accounts []service.Account) ([]service.Proxy, error) {
	if len(accounts) == 0 {
		return []service.Proxy{}, nil
	}

	seen := make(map[int64]struct{})
	ids := make([]int64, 0)
	for i := range accounts {
		if accounts[i].ProxyID == nil {
			continue
		}
		id := *accounts[i].ProxyID
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []service.Proxy{}, nil
	}

	return h.adminService.GetProxiesByIDs(ctx, ids)
}

func resolveDataGroups(accounts []service.Account, includeGroups bool) (map[int64]*service.Group, []DataGroup) {
	groupByID := make(map[int64]*service.Group)
	if !includeGroups {
		return groupByID, []DataGroup{}
	}

	dataGroups := make([]DataGroup, 0)
	for i := range accounts {
		for _, group := range accountGroupsForExport(accounts[i]) {
			if group == nil || group.ID <= 0 {
				continue
			}
			if _, exists := groupByID[group.ID]; exists {
				continue
			}
			groupByID[group.ID] = group
			dataGroups = append(dataGroups, DataGroup{
				Name:             group.Name,
				Description:      group.Description,
				Platform:         group.Platform,
				RateMultiplier:   normalizeDataGroupRateMultiplier(group.RateMultiplier),
				IsExclusive:      group.IsExclusive,
				Status:           group.Status,
				SubscriptionType: group.SubscriptionType,
			})
		}
	}
	return groupByID, dataGroups
}

func accountGroupsForExport(acc service.Account) []*service.Group {
	if len(acc.Groups) > 0 {
		return acc.Groups
	}
	groups := make([]*service.Group, 0, len(acc.AccountGroups))
	for i := range acc.AccountGroups {
		if acc.AccountGroups[i].Group != nil {
			groups = append(groups, acc.AccountGroups[i].Group)
		}
	}
	return groups
}

func resolveAccountGroupNames(acc service.Account, groupByID map[int64]*service.Group, includeGroups bool) []string {
	if !includeGroups {
		return nil
	}
	names := make([]string, 0)
	seen := map[string]struct{}{}
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, group := range accountGroupsForExport(acc) {
		if group != nil {
			appendName(group.Name)
		}
	}
	for _, id := range acc.GroupIDs {
		if group := groupByID[id]; group != nil {
			appendName(group.Name)
		}
	}
	return names
}

func normalizeDataGroupRateMultiplier(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}

func parseAccountIDs(c *gin.Context) ([]int64, error) {
	values := c.QueryArray("ids")
	if len(values) == 0 {
		raw := strings.TrimSpace(c.Query("ids"))
		if raw != "" {
			values = []string{raw}
		}
	}
	if len(values) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(values))
	for _, item := range values {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("invalid account id: %s", part)
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func parseIncludeProxies(c *gin.Context) (bool, error) {
	return parseBoolQueryDefault(c.Query("include_proxies"), true, "include_proxies")
}

func parseIncludeGroups(c *gin.Context) (bool, error) {
	return parseBoolQueryDefault(c.Query("include_groups"), true, "include_groups")
}

func parseBoolQueryDefault(raw string, defaultValue bool, name string) (bool, error) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return defaultValue, nil
	}
	switch value {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return defaultValue, fmt.Errorf("invalid %s value: %s", name, raw)
	}
}

func validateDataHeader(payload DataPayload) error {
	if payload.Type != "" && payload.Type != dataType && payload.Type != legacyDataType {
		return fmt.Errorf("unsupported data type: %s", payload.Type)
	}
	if payload.Version != 0 && payload.Version != dataVersion {
		return fmt.Errorf("unsupported data version: %d", payload.Version)
	}
	if payload.Proxies == nil {
		return errors.New("proxies is required")
	}
	if payload.Groups == nil {
		payload.Groups = []DataGroup{}
	}
	if payload.Accounts == nil {
		return errors.New("accounts is required")
	}
	return nil
}

func validateDataProxy(item DataProxy) error {
	if strings.TrimSpace(item.Protocol) == "" {
		return errors.New("proxy protocol is required")
	}
	if strings.TrimSpace(item.Host) == "" {
		return errors.New("proxy host is required")
	}
	if item.Port <= 0 || item.Port > 65535 {
		return errors.New("proxy port is invalid")
	}
	switch item.Protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy protocol is invalid: %s", item.Protocol)
	}
	if item.Status != "" {
		normalizedStatus := normalizeProxyStatus(item.Status)
		if normalizedStatus != service.StatusActive && normalizedStatus != "inactive" {
			return fmt.Errorf("proxy status is invalid: %s", item.Status)
		}
	}
	return nil
}

func validateDataGroup(item DataGroup) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("group name is required")
	}
	if strings.TrimSpace(item.Platform) == "" {
		return errors.New("group platform is required")
	}
	if item.RateMultiplier < 0 {
		return errors.New("group rate_multiplier must be >= 0")
	}
	if item.Status != "" {
		normalizedStatus := normalizeProxyStatus(item.Status)
		if normalizedStatus != service.StatusActive && normalizedStatus != "inactive" {
			return fmt.Errorf("group status is invalid: %s", item.Status)
		}
	}
	return nil
}

func resolveImportGroupIDs(groupNames []string, platform string, groupKeyToID, groupNameToID map[string]int64) ([]int64, error) {
	if len(groupNames) == 0 {
		return nil, nil
	}
	groupIDs := make([]int64, 0, len(groupNames))
	seen := map[int64]struct{}{}
	for _, rawName := range groupNames {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		id, ok := groupKeyToID[buildGroupKey(platform, name)]
		if !ok {
			id, ok = groupNameToID[name]
		}
		if !ok || id <= 0 {
			return nil, fmt.Errorf("group not found: %s", name)
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		groupIDs = append(groupIDs, id)
	}
	return groupIDs, nil
}

func findExistingDataAccount(item DataAccount, accounts []service.Account) *service.Account {
	itemKeys := stableAccountMatchKeys(item.Platform, item.Type, item.Credentials, item.Extra)
	if len(itemKeys) == 0 {
		itemKeys = []string{fallbackAccountNameKey(item.Platform, item.Type, item.Name)}
	}
	for i := range accounts {
		acc := accounts[i]
		if acc.Platform != item.Platform || acc.Type != item.Type {
			continue
		}
		existingKeys := stableAccountMatchKeys(acc.Platform, acc.Type, acc.Credentials, acc.Extra)
		if len(itemKeys) == 1 && itemKeys[0] == fallbackAccountNameKey(item.Platform, item.Type, item.Name) && len(existingKeys) == 0 {
			existingKeys = []string{fallbackAccountNameKey(acc.Platform, acc.Type, acc.Name)}
		}
		for _, itemKey := range itemKeys {
			for _, existingKey := range existingKeys {
				if itemKey == existingKey {
					return &accounts[i]
				}
			}
		}
	}
	return nil
}

func stableAccountMatchKeys(platform, accountType string, credentials, extra map[string]any) []string {
	prefix := strings.ToLower(strings.TrimSpace(platform)) + "|" + strings.ToLower(strings.TrimSpace(accountType)) + "|"
	keys := make([]string, 0, 8)
	add := func(kind, value string) {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			return
		}
		keys = append(keys, prefix+kind+"|"+value)
	}
	for _, field := range []string{
		"chatgpt_account_id",
		"chatgpt_user_id",
		"account_id",
		"user_id",
		"email",
		"email_address",
		"sub",
		"organization_id",
	} {
		add(field, dataMapString(credentials, field))
		add(field, dataMapString(extra, field))
	}
	return keys
}

func fallbackAccountNameKey(platform, accountType, name string) string {
	return strings.ToLower(strings.TrimSpace(platform)) + "|" + strings.ToLower(strings.TrimSpace(accountType)) + "|name|" + strings.ToLower(strings.TrimSpace(name))
}

func dataMapString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	switch v := values[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func validateDataAccount(item DataAccount) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("account name is required")
	}
	if strings.TrimSpace(item.Platform) == "" {
		return errors.New("account platform is required")
	}
	if strings.TrimSpace(item.Type) == "" {
		return errors.New("account type is required")
	}
	if len(item.Credentials) == 0 {
		return errors.New("account credentials is required")
	}
	switch item.Type {
	case service.AccountTypeOAuth, service.AccountTypeSetupToken, service.AccountTypeAPIKey, service.AccountTypeUpstream:
	default:
		return fmt.Errorf("account type is invalid: %s", item.Type)
	}
	if item.RateMultiplier != nil && *item.RateMultiplier < 0 {
		return errors.New("rate_multiplier must be >= 0")
	}
	if item.Concurrency < 0 {
		return errors.New("concurrency must be >= 0")
	}
	if item.Priority < 0 {
		return errors.New("priority must be >= 0")
	}
	return nil
}

func defaultProxyName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "imported-proxy"
	}
	return name
}

// enrichCredentialsFromIDToken performs best-effort extraction of user info fields
// (email, plan_type, chatgpt_account_id, etc.) from id_token in credentials.
// Only applies to OpenAI OAuth accounts. Skips expired token errors silently.
// Existing credential values are never overwritten — only missing fields are filled.
func enrichCredentialsFromIDToken(item *DataAccount) {
	if item.Credentials == nil {
		return
	}
	// Only enrich OpenAI OAuth accounts
	platform := strings.ToLower(strings.TrimSpace(item.Platform))
	if platform != service.PlatformOpenAI {
		return
	}
	if strings.ToLower(strings.TrimSpace(item.Type)) != service.AccountTypeOAuth {
		return
	}

	idToken, _ := item.Credentials["id_token"].(string)
	if strings.TrimSpace(idToken) == "" {
		return
	}

	// DecodeIDToken skips expiry validation — safe for imported data
	claims, err := openai.DecodeIDToken(idToken)
	if err != nil {
		slog.Debug("import_enrich_id_token_decode_failed", "account", item.Name, "error", err)
		return
	}

	userInfo := claims.GetUserInfo()
	if userInfo == nil {
		return
	}

	// Fill missing fields only (never overwrite existing values)
	setIfMissing := func(key, value string) {
		if value == "" {
			return
		}
		if existing, _ := item.Credentials[key].(string); existing == "" {
			item.Credentials[key] = value
		}
	}

	setIfMissing("email", userInfo.Email)
	setIfMissing("plan_type", userInfo.PlanType)
	setIfMissing("chatgpt_account_id", userInfo.ChatGPTAccountID)
	setIfMissing("chatgpt_user_id", userInfo.ChatGPTUserID)
	setIfMissing("organization_id", userInfo.OrganizationID)
}

func normalizeProxyStatus(status string) string {
	normalized := strings.TrimSpace(strings.ToLower(status))
	switch normalized {
	case "":
		return ""
	case service.StatusActive:
		return service.StatusActive
	case "inactive", service.StatusDisabled:
		return "inactive"
	default:
		return normalized
	}
}

package service

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const nvidiaAdaptiveThrottleSettingsTTL = 5 * time.Second

type nvidiaAdaptiveThrottleSettingsEntry struct {
	settings *NVIDIAAdaptiveThrottleSettings
	loadedAt time.Time
}

type nvidiaAdaptiveThrottleSettingsCache struct {
	value atomic.Value
	group singleflight.Group
}

func newNVIDIAAdaptiveThrottleSettingsCache() *nvidiaAdaptiveThrottleSettingsCache {
	return &nvidiaAdaptiveThrottleSettingsCache{}
}

func (s *RateLimitService) getNVIDIAAdaptiveThrottleSettings(ctx context.Context) *NVIDIAAdaptiveThrottleSettings {
	defaults := DefaultNVIDIAAdaptiveThrottleSettings()
	if s == nil || s.settingService == nil {
		return defaults
	}
	if s.nvidiaThrottleSettings == nil {
		s.nvidiaThrottleSettings = newNVIDIAAdaptiveThrottleSettingsCache()
	}
	now := s.nvidiaThrottleNow()
	if cached := s.nvidiaThrottleSettings.load(now); cached != nil {
		return cached
	}

	value, _, _ := s.nvidiaThrottleSettings.group.Do("settings", func() (any, error) {
		if cached := s.nvidiaThrottleSettings.load(s.nvidiaThrottleNow()); cached != nil {
			return cached, nil
		}
		settings, err := s.settingService.GetNVIDIAAdaptiveThrottleSettings(ctx)
		if err != nil {
			return defaults, nil
		}
		s.nvidiaThrottleSettings.value.Store(nvidiaAdaptiveThrottleSettingsEntry{
			settings: settings,
			loadedAt: s.nvidiaThrottleNow(),
		})
		return settings, nil
	})
	settings, ok := value.(*NVIDIAAdaptiveThrottleSettings)
	if !ok || settings == nil {
		return defaults
	}
	return settings
}

// nvidiaAdaptiveThrottleL1Jitter 返回当前配置的 L1 TTL 抖动窗口; 0 表示关闭抖动.
// 走 5s 缓存的 settings getter, 不会每次打 DB.
func (s *RateLimitService) nvidiaAdaptiveThrottleL1Jitter(ctx context.Context) time.Duration {
	return s.getNVIDIAAdaptiveThrottleSettings(ctx).L1Jitter
}

func (c *nvidiaAdaptiveThrottleSettingsCache) load(now time.Time) *NVIDIAAdaptiveThrottleSettings {
	if c == nil {
		return nil
	}
	value := c.value.Load()
	if value == nil {
		return nil
	}
	entry, ok := value.(nvidiaAdaptiveThrottleSettingsEntry)
	if !ok || entry.settings == nil || now.Sub(entry.loadedAt) >= nvidiaAdaptiveThrottleSettingsTTL {
		return nil
	}
	return entry.settings
}

func (s *RateLimitService) nvidiaThrottleCurrentTime() time.Time {
	if s != nil && s.nvidiaThrottleNow != nil {
		return s.nvidiaThrottleNow()
	}
	return time.Now()
}

// GetNVIDIAAdaptiveThrottleShortWait 返回当前配置的短等待上限，供调度器填充 BlockedError。
// 返回 0 表示短等待关闭；调用方据此决定 handler 层是否等待。
func (s *RateLimitService) GetNVIDIAAdaptiveThrottleShortWait(ctx context.Context) time.Duration {
	if s == nil {
		return 0
	}
	return s.getNVIDIAAdaptiveThrottleSettings(ctx).ShortWait
}

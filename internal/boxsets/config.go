package boxsets

import (
	"fmt"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Config entry and field names, as manifest.json declares them.
const (
	configKey      = "settings"
	fieldAPIKey    = "api_key"
	fieldGroupName = "group_name"
	fieldSiloURL   = "silo_url"

	defaultGroupName = "Box Sets"
)

// Config is the plugin's settings.
type Config struct {
	// APIKey is a Silo API key of an administrator account.
	APIKey string
	// GroupName names the collection group the box sets go in.
	GroupName string
	// SiloURL overrides the address the host reports for Silo's API.
	SiloURL string
}

// ParseConfig reads Configure entries. Missing values take their defaults;
// it fails only on values of the wrong type.
func ParseConfig(entries []*pluginv1.ConfigEntry) (Config, error) {
	cfg := Config{GroupName: defaultGroupName}
	for _, entry := range entries {
		if entry.GetKey() != configKey {
			continue
		}
		values := entry.GetValue().AsMap()
		for field, dst := range map[string]*string{fieldAPIKey: &cfg.APIKey, fieldGroupName: &cfg.GroupName, fieldSiloURL: &cfg.SiloURL} {
			raw, ok := values[field]
			if !ok || raw == nil {
				continue
			}
			s, ok := raw.(string)
			if !ok {
				return Config{}, fmt.Errorf("%s.%s must be a string", configKey, field)
			}
			if s = strings.TrimSpace(s); s != "" {
				*dst = s
			}
		}
	}
	cfg.SiloURL = strings.TrimRight(cfg.SiloURL, "/")
	return cfg, nil
}

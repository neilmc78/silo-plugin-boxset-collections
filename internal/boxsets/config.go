package boxsets

import (
	"fmt"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Config entry and field names, as manifest.json declares them.
const (
	configKey      = "settings"
	fieldAPIKey    = "api_key"
	fieldGroupName = "group_name"
	fieldSiloURL   = "silo_url"

	fieldTVDBAPIKey      = "tvdb_api_key"
	fieldTVDBPin         = "tvdb_pin"
	fieldSeriesGroupName = "series_group_name"
	fieldSeriesMinShows  = "series_min_shows"
	fieldTVFranchises    = "tv_franchises"

	defaultGroupName       = "Box Sets"
	defaultSeriesGroupName = "Franchises"
	defaultSeriesMinShows  = 2
	maxSeriesMinShows      = 20
)

// Config is the plugin's settings.
type Config struct {
	// APIKey is a Silo API key of an administrator account.
	APIKey string
	// GroupName names the collection group the box sets go in.
	GroupName string
	// SiloURL overrides the address the host reports for Silo's API.
	SiloURL string

	// TVDBAPIKey is the install's own TheTVDB key; series franchises are
	// synced only when it is set. TVDBPin is its PIN, for subscriber keys.
	TVDBAPIKey string
	TVDBPin    string
	// SeriesGroupName names the collection group series franchises go in.
	SeriesGroupName string
	// TVFranchises is the settings switch for series franchises; they run
	// only when it is on and a TheTVDB key is set.
	TVFranchises bool
	// SeriesMinShows is how many of a list's series a library needs before
	// the list becomes a collection there.
	SeriesMinShows int
}

// ParseConfig reads Configure entries. Missing values take their defaults;
// it fails only on values of the wrong type.
func ParseConfig(entries []*pluginv1.ConfigEntry) (Config, error) {
	// TV franchises default to on, so an install that set a TheTVDB key
	// before the switch existed keeps them.
	cfg := Config{GroupName: defaultGroupName, SeriesGroupName: defaultSeriesGroupName, SeriesMinShows: defaultSeriesMinShows, TVFranchises: true}
	for _, entry := range entries {
		if entry.GetKey() != configKey {
			continue
		}
		values := entry.GetValue().AsMap()
		if raw, ok := values[fieldTVFranchises]; ok && raw != nil {
			switch v := raw.(type) {
			case bool:
				cfg.TVFranchises = v
			case string:
				on, err := strconv.ParseBool(strings.TrimSpace(v))
				if err != nil {
					return Config{}, fmt.Errorf("%s.%s must be true or false", configKey, fieldTVFranchises)
				}
				cfg.TVFranchises = on
			default:
				return Config{}, fmt.Errorf("%s.%s must be true or false", configKey, fieldTVFranchises)
			}
		}
		if raw, ok := values[fieldSeriesMinShows]; ok && raw != nil {
			n, err := minShows(raw)
			if err != nil {
				return Config{}, err
			}
			if n > 0 {
				cfg.SeriesMinShows = n
			}
		}
		for field, dst := range map[string]*string{
			fieldAPIKey: &cfg.APIKey, fieldGroupName: &cfg.GroupName, fieldSiloURL: &cfg.SiloURL,
			fieldTVDBAPIKey: &cfg.TVDBAPIKey, fieldTVDBPin: &cfg.TVDBPin, fieldSeriesGroupName: &cfg.SeriesGroupName,
		} {
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

// minShows reads series_min_shows, which the admin form may send as a
// number or as text. It is kept between 2 and maxSeriesMinShows; 0 means
// the default.
func minShows(raw any) (int, error) {
	var n int
	switch v := raw.(type) {
	case float64:
		n = int(v)
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, nil
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("%s.%s must be a whole number", configKey, fieldSeriesMinShows)
		}
		n = parsed
	default:
		return 0, fmt.Errorf("%s.%s must be a number", configKey, fieldSeriesMinShows)
	}
	if n <= 0 {
		return 0, nil
	}
	return min(max(n, 2), maxSeriesMinShows), nil
}

// SeriesEnabled reports whether series franchises run: the switch is on and
// a TheTVDB key is set.
func (c Config) SeriesEnabled() bool { return c.TVFranchises && c.TVDBAPIKey != "" }

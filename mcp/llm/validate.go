package llm

import "strings"

// ValidateSettings returns the list of required settings-profile fields missing
// from the request. The frontend profile must supply all of them.
func ValidateSettings(cfg LLMSettings) []string {
	var missing []string
	if strings.TrimSpace(cfg.Provider) == "" {
		missing = append(missing, "settings.provider")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		missing = append(missing, "settings.model")
	}
	if strings.TrimSpace(cfg.AccessToken) == "" {
		missing = append(missing, "settings.access_token")
	}
	return missing
}

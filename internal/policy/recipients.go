package policy

import "github.com/arm2arm/open-llm-router/internal/config"

// Recipient contains only operator-designated public information. Internal
// backend names, IDs, URLs, paths and credentials must never be used as fallbacks.
type Recipient struct {
	Organization string `json:"organization"`
	PrivacyURL   string `json:"privacy_url"`
}

// ModelRecipients uses the active configuration and the same backend access
// rules as inference. Health/capacity must not hide initial or retry destinations.
// Callers must authorize the canonical model before calling this function.
func ModelRecipients(cfg *config.Config, name string, groups []string, protected bool) ([]Recipient, bool) {
	recipients := []Recipient{}
	seen := map[Recipient]bool{}
	complete := true
	for _, replica := range cfg.Models[name].Pool {
		if !BackendAllowed(cfg, replica.Backend, groups, protected) {
			continue
		}
		recipient := Recipient{Organization: replica.PublicOrganization, PrivacyURL: replica.PrivacyURL}
		if recipient.Organization == "" {
			recipient.Organization = cfg.Portal.RecipientLabels[replica.URL]
		}
		if recipient.Organization == "" || recipient.PrivacyURL == "" {
			complete = false
		}
		if !seen[recipient] {
			recipients = append(recipients, recipient)
			seen[recipient] = true
		}
	}
	return recipients, complete && len(recipients) > 0
}

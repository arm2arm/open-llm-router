package config

// BackendRate is a shared backend budget. Nil values mean unlimited.
type BackendRate struct {
	Name          string `json:"name"`
	RPM           *int64 `json:"rpm"`
	TPM           *int64 `json:"tpm"`
	MaxConcurrent *int   `json:"max_concurrent"`
}

func (r BackendRate) Limited() bool { return r.RPM != nil || r.TPM != nil || r.MaxConcurrent != nil }

type BackendPolicy struct {
	// Nil preserves legacy model-only authorization; an empty list denies
	// ordinary users. Administrators retain access.
	Groups []string
	Rate   BackendRate
}

func DefaultBackendPresets() map[string]BackendRate {
	rpm, tpm, concurrency := int64(60), int64(60000), 8
	smallRPM, smallTPM, smallConcurrency := int64(20), int64(20000), 2
	return map[string]BackendRate{
		"unlimited": {Name: "Unlimited"},
		"standard":  {Name: "Standard", RPM: &rpm, TPM: &tpm, MaxConcurrent: &concurrency},
		"small":     {Name: "Small cluster", RPM: &smallRPM, TPM: &smallTPM, MaxConcurrent: &smallConcurrency},
	}
}

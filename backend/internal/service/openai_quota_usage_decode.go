package service

import "encoding/json"

// Preserve field presence for the user projection without changing the scalar
// fields consumed by existing administrator and scheduling code.
func (w *OpenAIRateLimitWindow) UnmarshalJSON(data []byte) error {
	type plain OpenAIRateLimitWindow
	var decoded struct {
		plain
		UsedPercent *float64 `json:"used_percent"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*w = OpenAIRateLimitWindow(decoded.plain)
	w.usedPercentMissing = decoded.UsedPercent == nil
	if decoded.UsedPercent != nil {
		w.UsedPercent = *decoded.UsedPercent
	}
	return nil
}

func (c *OpenAIRateLimitResetCredits) UnmarshalJSON(data []byte) error {
	type plain OpenAIRateLimitResetCredits
	var decoded struct {
		plain
		AvailableCount *int `json:"available_count"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = OpenAIRateLimitResetCredits(decoded.plain)
	c.availableCountMissing = decoded.AvailableCount == nil
	if decoded.AvailableCount != nil {
		c.AvailableCount = *decoded.AvailableCount
	}
	return nil
}

func (c *OpenAICredits) UnmarshalJSON(data []byte) error {
	type plain OpenAICredits
	var decoded struct {
		plain
		HasCredits *bool `json:"has_credits"`
		Unlimited  *bool `json:"unlimited"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = OpenAICredits(decoded.plain)
	c.statusMissing = decoded.HasCredits == nil || decoded.Unlimited == nil
	if decoded.HasCredits != nil {
		c.HasCredits = *decoded.HasCredits
	}
	if decoded.Unlimited != nil {
		c.Unlimited = *decoded.Unlimited
	}
	return nil
}

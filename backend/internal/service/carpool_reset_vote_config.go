package service

import infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"

const (
	ResetVoteMin7DPercentExtraKey = "reset_vote_min_7d_percent"
	DefaultResetVoteMin7DPercent  = 1.0
)

// ParseResetVoteMin7DPercent validates the percentage stored in account extra.
func ParseResetVoteMin7DPercent(value any) (float64, error) {
	percent, ok := parseOpenAIAutoResetThreshold(value)
	if !ok || !(percent >= 0.1 && percent <= 100) {
		return 0, infraerrors.BadRequest("RESET_VOTE_MIN_7D_PERCENT_INVALID", "Reset vote minimum must be between 0.1% and 100%")
	}
	return percent, nil
}

func normalizeResetVoteExtra(platform, accountType string, isShadow bool, extra map[string]any) error {
	value, configured := extra[ResetVoteMin7DPercentExtraKey]
	if !configured {
		return nil
	}
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth || isShadow {
		return infraerrors.BadRequest("RESET_VOTE_ACCOUNT_INVALID", "Reset vote minimum is only supported for OpenAI OAuth parent accounts")
	}
	percent, err := ParseResetVoteMin7DPercent(value)
	if err != nil {
		return err
	}
	extra[ResetVoteMin7DPercentExtraKey] = percent
	return nil
}

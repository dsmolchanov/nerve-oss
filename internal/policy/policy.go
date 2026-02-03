package policy

import (
	"errors"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	ID                string   `yaml:"id"`
	Name              string   `yaml:"name"`
	Version           int      `yaml:"version"`
	AllowedTones      []string `yaml:"allowed_tones"`
	ForbiddenPhrases  []string `yaml:"forbidden_phrases"`
	RequiredDiscl     []string `yaml:"required_disclosures"`
	OutboundAllowlist []string `yaml:"outbound_domain_allowlist"`
	MaxReplyLength    int      `yaml:"max_reply_length_chars"`
	Redactions        struct {
		Patterns    []string `yaml:"patterns"`
		Replacement string   `yaml:"replacement"`
	} `yaml:"redactions"`
	Approval struct {
		RequiredWhen       []string `yaml:"required_when"`
		ConfidenceThreshold float64  `yaml:"confidence_threshold"`
	} `yaml:"approval"`
}

type Result struct {
	Allowed             bool
	ViolationLevel      string
	Reason              string
	SuggestedRedaction  string
	RiskFlags           []string
	NeedsApproval       bool
	RedactionsApplied   []string
}

func Load(path string) (Policy, error) {
	var p Policy
	if path == "" {
		return p, errors.New("missing policy path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := yaml.Unmarshal(data, &p); err != nil {
		return p, err
	}
	return p, nil
}

func Evaluate(draft string, policy Policy) (string, Result) {
	res := Result{Allowed: true}
	text := draft

	for _, phrase := range policy.ForbiddenPhrases {
		if phrase == "" {
			continue
		}
		if strings.Contains(strings.ToLower(text), strings.ToLower(phrase)) {
			res.Allowed = false
			res.ViolationLevel = "critical"
			res.Reason = "Draft contains forbidden phrase: " + phrase
			res.RiskFlags = append(res.RiskFlags, "forbidden_phrase")
			return text, res
		}
	}

	for _, pattern := range policy.Redactions.Patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		if re.MatchString(text) {
			res.RiskFlags = append(res.RiskFlags, "contains_sensitive_data")
			res.RedactionsApplied = append(res.RedactionsApplied, pattern)
			replacement := policy.Redactions.Replacement
			if replacement == "" {
				replacement = "[REDACTED]"
			}
			text = re.ReplaceAllString(text, replacement)
		}
	}

	if policy.MaxReplyLength > 0 && len(text) > policy.MaxReplyLength {
		res.Allowed = false
		res.ViolationLevel = "critical"
		res.Reason = "Draft exceeds max reply length"
		res.RiskFlags = append(res.RiskFlags, "too_long")
		return text, res
	}

	for _, disclosure := range policy.RequiredDiscl {
		if disclosure == "" {
			continue
		}
		if !strings.Contains(text, disclosure) {
			res.RiskFlags = append(res.RiskFlags, "missing_disclosure")
			res.NeedsApproval = true
		}
	}

	if len(res.RiskFlags) > 0 && res.ViolationLevel == "" {
		res.ViolationLevel = "warning"
	}

	res.SuggestedRedaction = text
	return text, res
}

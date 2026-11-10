package retry

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	MaxRetries          map[Category]int           `yaml:"max_retries"`
	BackoffBase         map[Category]time.Duration `yaml:"-"`
	BackoffBaseMs       map[Category]int           `yaml:"backoff_base_ms"`
	BackoffFactor       float64                    `yaml:"backoff_factor"`
	MaxBackoffMs        int                        `yaml:"max_backoff_ms"`
	JitterRatio         float64                    `yaml:"jitter_ratio"`
	AcceptanceMaxFails  int                        `yaml:"acceptance_max_fails"`
	SilentThresholdSecs int                        `yaml:"silent_threshold_seconds"`
	QuotaHardFailSecs   int                        `yaml:"quota_hard_fail_seconds"`
}

func DefaultPolicy() Policy {
	return Policy{
		MaxRetries: map[Category]int{
			Network:           3,
			Runtime:           3,
			System:            3,
			Quota:             1,
			Timeout:           2,
			NoFinish:          2,
			InvalidToolResult: 2,
			VerificationError: 2,
			ContentFilter:     0,
		},
		BackoffBase: map[Category]time.Duration{
			Network:           time.Second,
			Runtime:           500 * time.Millisecond,
			System:            2 * time.Second,
			Quota:             0,
			Timeout:           0,
			NoFinish:          0,
			InvalidToolResult: 200 * time.Millisecond,
			VerificationError: 500 * time.Millisecond,
			ContentFilter:     0,
		},
		BackoffFactor:       2,
		MaxBackoffMs:        30000,
		JitterRatio:         0.2,
		AcceptanceMaxFails:  5,
		SilentThresholdSecs: 120,
		QuotaHardFailSecs:   30,
	}
}

type policyFile struct {
	MaxRetries          map[string]int `yaml:"max_retries"`
	BackoffBaseMs       map[string]int `yaml:"backoff_base_ms"`
	BackoffFactor       float64        `yaml:"backoff_factor"`
	MaxBackoffMs        int            `yaml:"max_backoff_ms"`
	JitterRatio         float64        `yaml:"jitter_ratio"`
	AcceptanceMaxFails  int            `yaml:"acceptance_max_fails"`
	SilentThresholdSecs int            `yaml:"silent_threshold_seconds"`
	QuotaHardFailSecs   int            `yaml:"quota_hard_fail_seconds"`
}

func LoadPolicy(path string) (Policy, error) {
	p := DefaultPolicy()
	if path == "" {
		return p, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	var f policyFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return p, err
	}
	for k, v := range f.MaxRetries {
		if Category(k).Valid() {
			p.MaxRetries[Category(k)] = v
		}
	}
	for k, v := range f.BackoffBaseMs {
		if Category(k).Valid() {
			p.BackoffBase[Category(k)] = time.Duration(v) * time.Millisecond
		}
	}
	if f.BackoffFactor > 0 {
		p.BackoffFactor = f.BackoffFactor
	}
	if f.MaxBackoffMs > 0 {
		p.MaxBackoffMs = f.MaxBackoffMs
	}
	if f.JitterRatio >= 0 {
		p.JitterRatio = f.JitterRatio
	}
	if f.AcceptanceMaxFails > 0 {
		p.AcceptanceMaxFails = f.AcceptanceMaxFails
	}
	if f.SilentThresholdSecs > 0 {
		p.SilentThresholdSecs = f.SilentThresholdSecs
	}
	if f.QuotaHardFailSecs > 0 {
		p.QuotaHardFailSecs = f.QuotaHardFailSecs
	}
	return p, nil
}

func (p Policy) MaxRetriesFor(c Category) int {
	if v, ok := p.MaxRetries[c]; ok {
		return v
	}
	return 0
}

func (p Policy) Retryable(c Category) bool {
	return p.MaxRetriesFor(c) > 0
}

func (p Policy) ExclusionReason(c Category) string {
	switch c {
	case ContentFilter:
		return "content_filter is not retryable"
	case Quota:
		return "quota exhaustion fails fast"
	}
	if p.MaxRetriesFor(c) == 0 {
		return "no retries configured for this category"
	}
	return ""
}

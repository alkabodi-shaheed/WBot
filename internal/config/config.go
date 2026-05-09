package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Port           string
	DatabaseURL    string
	TargetGroupJID string // raw single-group JID; empty when ScanAll is true
	ScanAll        bool   // true when TARGET_GROUP_JID is the sentinel "all" (case-insensitive)
	TargetKeywords []string
	PairToken      string
	LogLevel       string
	Timezone       string
}

func Load() (*Config, error) {
	c := &Config{
		Port:        getEnv("PORT", "10000"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		PairToken:   os.Getenv("PAIR_TOKEN"),
		LogLevel:    getEnv("LOG_LEVEL", "INFO"),
		Timezone:    getEnv("TZ", "UTC"),
	}

	// Resolve target scope: "all" (case-insensitive) means scan every chat;
	// any other non-empty value is treated as a specific JID and parsed downstream.
	targetRaw := strings.TrimSpace(os.Getenv("TARGET_GROUP_JID"))
	if strings.EqualFold(targetRaw, "all") {
		c.ScanAll = true
		c.TargetGroupJID = ""
	} else {
		c.TargetGroupJID = targetRaw
	}

	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if c.PairToken == "" {
		return nil, errors.New("PAIR_TOKEN is required")
	}

	kw := strings.TrimSpace(os.Getenv("TARGET_KEYWORDS"))
	if kw == "" {
		return nil, errors.New("TARGET_KEYWORDS is required (comma-separated)")
	}
	for _, k := range strings.Split(kw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			c.TargetKeywords = append(c.TargetKeywords, k)
		}
	}
	if len(c.TargetKeywords) == 0 {
		return nil, errors.New("TARGET_KEYWORDS parsed to empty list")
	}

	return c, nil
}

func (c *Config) Summary() string {
	target := c.TargetGroupJID
	if c.ScanAll {
		target = "<all-chats>"
	} else if target == "" {
		target = "<unset>"
	}
	return fmt.Sprintf(
		"port=%s target_group=%q keywords=%v log=%s tz=%s",
		c.Port, target, c.TargetKeywords, c.LogLevel, c.Timezone,
	)
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

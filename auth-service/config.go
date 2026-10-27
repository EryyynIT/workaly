package main

import (
	"errors"
	"net/url"
	"strconv"
	"time"
)

const requestTimeout = 5 * time.Second

type config struct {
	HTTPAddr        string
	UserServiceAddr string
	JWTSecret       []byte
	CookieSecure    bool
	PublicOrigin    string
	DatabaseURL     string
	DevMailLog      bool
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		HTTPAddr:        "127.0.0.1:8080",
		UserServiceAddr: "127.0.0.1:50051",
		CookieSecure:    true,
		PublicOrigin:    "http://127.0.0.1:8080",
		JWTSecret:       []byte(getenv("JWT_SECRET")),
		DatabaseURL:     getenv("AUTH_DATABASE_URL"),
	}
	if len(cfg.JWTSecret) < 32 {
		return config{}, errors.New("JWT_SECRET must contain at least 32 bytes; generate a random secret")
	}
	if cfg.DatabaseURL == "" {
		return config{}, errors.New("AUTH_DATABASE_URL is required")
	}
	if value := getenv("HTTP_ADDR"); value != "" {
		cfg.HTTPAddr = value
	}
	if value := getenv("USER_SERVICE_ADDR"); value != "" {
		cfg.UserServiceAddr = value
	}
	if value := getenv("COOKIE_SECURE"); value != "" {
		secure, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, errors.New("COOKIE_SECURE must be a boolean")
		}
		cfg.CookieSecure = secure
	}
	if value := getenv("PUBLIC_ORIGIN"); value != "" {
		cfg.PublicOrigin = value
	}
	origin, err := url.Parse(cfg.PublicOrigin)
	if err != nil || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return config{}, errors.New("PUBLIC_ORIGIN must be an http(s) origin without a path")
	}
	cfg.DevMailLog = getenv("DEV_MAIL_LOG") == "true"
	return cfg, nil
}

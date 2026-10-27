package main

import "errors"

type config struct {
	GRPCAddr    string
	DatabaseURL string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		GRPCAddr:    "127.0.0.1:50052",
		DatabaseURL: getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if value := getenv("GRPC_ADDR"); value != "" {
		cfg.GRPCAddr = value
	}
	return cfg, nil
}

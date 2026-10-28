package catalogkit

import "errors"

// Config is the common environment of a catalog service: a gRPC listen
// address and the DSN of the service-owned database.
type Config struct {
	GRPCAddr    string
	DatabaseURL string
}

// LoadConfig reads the shared environment. defaultAddr is the per-service
// gRPC listen address; GRPC_ADDR overrides it (compose sets 0.0.0.0).
func LoadConfig(getenv func(string) string, defaultAddr string) (Config, error) {
	cfg := Config{
		GRPCAddr:    defaultAddr,
		DatabaseURL: getenv("DATABASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if value := getenv("GRPC_ADDR"); value != "" {
		cfg.GRPCAddr = value
	}
	return cfg, nil
}

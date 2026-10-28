package catalogkit

import "testing"

func TestLoadConfig(t *testing.T) {
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://localhost:5432/workaly_job"
		}
		return ""
	}
	cfg, err := LoadConfig(getenv, "127.0.0.1:50052")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCAddr != "127.0.0.1:50052" {
		t.Fatalf("default GRPCAddr = %q", cfg.GRPCAddr)
	}
	if cfg.DatabaseURL != "postgres://localhost:5432/workaly_job" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
	}

	if _, err := LoadConfig(func(string) string { return "" }, "127.0.0.1:50052"); err == nil {
		t.Fatal("expected error without DATABASE_URL")
	}

	getenv = func(key string) string {
		switch key {
		case "DATABASE_URL":
			return "postgres://localhost:5432/workaly_task"
		case "GRPC_ADDR":
			return "0.0.0.0:50053"
		}
		return ""
	}
	cfg, err = LoadConfig(getenv, "127.0.0.1:50053")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GRPCAddr != "0.0.0.0:50053" {
		t.Fatalf("GRPCAddr override = %q", cfg.GRPCAddr)
	}
}

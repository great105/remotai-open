package config

import "testing"

func TestSelfHostedIsExplicitAndCannotEnableBilling(t *testing.T) {
	t.Setenv("JWT_HMAC_SECRET", "self-hosted-test-secret-0123456789")
	t.Setenv("TURN_ENABLED", "false")
	t.Setenv("BILLING_ENABLED", "false")
	t.Setenv("SELF_HOSTED", "false")
	cfg, err := FromEnv()
	if err != nil || cfg.SelfHosted {
		t.Fatalf("managed default: cfg=%v err=%v", cfg != nil, err)
	}
	t.Setenv("SELF_HOSTED", "true")
	cfg, err = FromEnv()
	if err != nil || !cfg.SelfHosted || cfg.BillingEnabled {
		t.Fatalf("self-hosted: cfg=%v err=%v", cfg != nil, err)
	}
	t.Setenv("BILLING_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("self-hosted deployment accepted billing")
	}
}

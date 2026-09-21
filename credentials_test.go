package main

import "testing"

func TestDiscordWebhookEnvironmentTakesPrecedence(t *testing.T) {
	t.Setenv("CONE_DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/test/value")
	value, err := loadDiscordWebhookURL()
	if err != nil {
		t.Fatal(err)
	}
	if value != "https://discord.com/api/webhooks/test/value" {
		t.Fatalf("Discord webhook URL = %q", value)
	}
}

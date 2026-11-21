package handler

import (
	"strings"
	"testing"

	"weknora-tokendance/pkg/model"
)

func TestResolveDeliveryBuildInputsUsesOnlyConfirmedPublicValues(t *testing.T) {
	const dockerfile = "FROM node:24\nARG PUBLIC_TITLE\nRUN npm run build\n"
	plan := `{
		"config_format": 1,
		"effective_configs": [{"key":"SITE_TITLE", "value":"confirmed title"}],
		"contract": {
			"configs":[{"key":"SITE_TITLE"}],
			"build_inputs":[{"arg":"PUBLIC_TITLE", "key":"SITE_TITLE", "evidence":["Dockerfile:2"]}]
		}
	}`
	configs := []model.AppConfigItem{
		{Key: "SITE_TITLE", Value: "confirmed title"},
		{Key: "DB_PASSWORD", Value: "runtime-only", Secret: true},
	}
	args, err := resolveDeliveryBuildInputs([]byte(plan), configs, dockerfile)
	if err != nil || len(args) != 1 || args["PUBLIC_TITLE"] != "confirmed title" {
		t.Fatalf("only the confirmed argument should be sent: %#v (%v)", args, err)
	}
	tests := map[string]struct {
		plan string
		want string
	}{
		"changed value":        {strings.Replace(plan, "confirmed title", "different", 1), "differs from the confirmed value"},
		"wrong file citation":  {strings.Replace(plan, "Dockerfile:2", "README.md:2", 1), "must cite the archived Dockerfile"},
		"missing line":         {strings.Replace(plan, "Dockerfile:2", "Dockerfile:20", 1), "must cite the archived Dockerfile"},
		"sensitive config":     {strings.Replace(plan, `"configs":[{"key":"SITE_TITLE"}]`, `"configs":[{"key":"SITE_TITLE","secret":true}]`, 1), "cannot use credentials"},
		"authentication input": {strings.Replace(plan, `"configs":[`, `"auth_profile":{"password_env":"SITE_TITLE"},"configs":[`, 1), "cannot use credentials"},
		"duplicate config":     {strings.Replace(plan, `"effective_configs": [`, `"effective_configs": [{"key":"SITE_TITLE","value":"different"},`, 1), "conflicting confirmed values"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveDeliveryBuildInputs([]byte(tc.plan), configs, dockerfile); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

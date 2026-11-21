package handler

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"
)

var buildArgName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var buildArgDockerfileCitation = regexp.MustCompile(`^Dockerfile:([1-9][0-9]{0,8})(?:-([1-9][0-9]{0,8}))?$`)

func hasBuildDockerfileCitation(evidence []string, dockerfile string) bool {
	if dockerfile == "" {
		return false
	}
	lines := 1 + strings.Count(strings.TrimSuffix(dockerfile, "\n"), "\n")
	for _, citation := range evidence {
		parts := buildArgDockerfileCitation.FindStringSubmatch(citation)
		if parts == nil {
			continue
		}
		start, _ := strconv.Atoi(parts[1])
		end := start
		if parts[2] != "" {
			end, _ = strconv.Atoi(parts[2])
		}
		if start <= end && end <= lines {
			return true
		}
	}
	return false
}

// Resolve from the archived, user-confirmed values. A retry must not resolve a new resource value.
func resolveDeliveryBuildInputs(data []byte, items []model.AppConfigItem, dockerfile string) (map[string]string, error) {
	type authReferences struct {
		User     string `json:"username_env"`
		Password string `json:"password_env"`
		Token    string `json:"token_env"`
	}
	type authRoute struct {
		Auth struct {
			Env string `json:"env"`
		} `json:"auth"`
	}
	var plan struct {
		Format           int                   `json:"config_format"`
		EffectiveConfigs []model.AppConfigItem `json:"effective_configs"`
		Contract         struct {
			Configs     []confirmedConfigDeclaration `json:"configs"`
			BuildInputs []struct {
				Arg      string   `json:"arg"`
				Key      string   `json:"key"`
				Evidence []string `json:"evidence"`
			} `json:"build_inputs"`
			AuthProfile       authReferences             `json:"auth_profile"`
			AuthProfiles      map[string]authReferences  `json:"auth_profiles"`
			CredentialSources map[string]json.RawMessage `json:"credential_sources"`
			APIs              []authRoute                `json:"apis"`
			Pages             []authRoute                `json:"pages"`
			AuthRules         []authRoute                `json:"auth_rules"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("invalid build input declarations: %w", err)
	}
	contract := plan.Contract
	if len(contract.BuildInputs) == 0 {
		return nil, nil
	}
	if plan.Format != 1 || plan.EffectiveConfigs == nil || len(contract.BuildInputs) > 64 {
		return nil, fmt.Errorf("build inputs require confirmed configuration and support at most 64 arguments")
	}
	credentials := make(map[string]bool, len(contract.CredentialSources))
	for key := range contract.CredentialSources {
		credentials[key] = true
	}
	rememberProfile := func(profile authReferences) {
		credentials[profile.User], credentials[profile.Password], credentials[profile.Token] = true, true, true
	}
	rememberProfile(contract.AuthProfile)
	for _, profile := range contract.AuthProfiles {
		rememberProfile(profile)
	}
	for _, routes := range [][]authRoute{contract.APIs, contract.Pages, contract.AuthRules} {
		for _, route := range routes {
			credentials[route.Auth.Env] = true
		}
	}
	declared := make(map[string]confirmedConfigDeclaration, len(contract.Configs))
	for _, item := range contract.Configs {
		if _, found := declared[item.Key]; found {
			return nil, fmt.Errorf("configuration %s is declared more than once", item.Key)
		}
		declared[item.Key] = item
	}
	actual := make(map[string]model.AppConfigItem, len(items))
	for _, item := range items {
		actual[item.Key] = item
	}
	effective := make(map[string]model.AppConfigItem, len(plan.EffectiveConfigs))
	for _, item := range plan.EffectiveConfigs {
		if _, found := effective[item.Key]; found {
			return nil, fmt.Errorf("configuration %s has conflicting confirmed values", item.Key)
		}
		effective[item.Key] = item
	}
	args := make(map[string]string, len(contract.BuildInputs))
	size := 0
	for _, input := range contract.BuildInputs {
		if _, exists := args[input.Arg]; exists || !buildArgName.MatchString(input.Arg) || !buildArgName.MatchString(input.Key) {
			return nil, fmt.Errorf("invalid or duplicate build argument declaration")
		}
		if !hasBuildDockerfileCitation(input.Evidence, dockerfile) {
			return nil, fmt.Errorf("build argument %s must cite the archived Dockerfile", input.Arg)
		}
		config, declaredKey := declared[input.Key]
		item, configuredKey := actual[input.Key]
		confirmed, confirmedKey := effective[input.Key]
		if !declaredKey || !configuredKey || !confirmedKey || item.ResourceID != confirmed.ResourceID || resourceEnvKey(item) != resourceEnvKey(confirmed) {
			return nil, fmt.Errorf("build argument %s requires a confirmed value for configuration %s", input.Arg, input.Key)
		}
		if item.ResourceID == "" && item.Value != confirmed.Value {
			return nil, fmt.Errorf("build input %s differs from the confirmed value", input.Key)
		}
		endpoint, _ := url.Parse(confirmed.Value)
		if config.Secret || item.Secret || confirmed.Secret || credentials[input.Key] ||
			appSensitiveKey.MatchString(input.Key) || appSensitiveKey.MatchString(input.Arg) ||
			appSensitiveKey.MatchString(item.ResourceKey) || strings.HasPrefix(confirmed.Value, crypto.EncPrefix) ||
			(endpoint != nil && endpoint.Host != "" && endpoint.User != nil) {
			return nil, fmt.Errorf("build argument %s cannot use credentials; the build secret transport is unavailable", input.Arg)
		}
		size += len(input.Arg) + len(confirmed.Value)
		if size > 64*1024 || len(confirmed.Value) > 8*1024 || strings.ContainsRune(confirmed.Value, 0) {
			return nil, fmt.Errorf("invalid or oversized confirmed build argument %s", input.Arg)
		}
		args[input.Arg] = confirmed.Value
	}
	return args, nil
}

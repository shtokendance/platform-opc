package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

var deliveryPathParameter = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_-]*\}`)

type deliveryProbeContract struct {
	Contract struct {
		Verification struct {
			Targets []struct {
				ID        string `json:"id"`
				Kind      string `json:"kind"`
				Method    string `json:"method"`
				Path      string `json:"path"`
				Operation string `json:"operation"`
			} `json:"targets"`
		} `json:"verification"`
		APIs []struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Path   string `json:"path"`
			Effect string `json:"effect"`
		} `json:"apis"`
	} `json:"contract"`
}

func (p deliveryProbeContract) probeKind(id string, probe deploy.CandidateProbe) (string, bool) {
	for _, target := range p.Contract.Verification.Targets {
		if target.ID != id {
			continue
		}
		switch target.Kind {
		case "http":
			method := strings.ToUpper(target.Method)
			if method == "" {
				method = "GET"
			}
			return "http", (method == "GET" || method == "HEAD") && probe.Method == method && probe.Path == target.Path
		case "operation":
			actual, err := url.ParseRequestURI(probe.Path)
			if err != nil {
				return "", false
			}
			for _, api := range p.Contract.APIs {
				if api.ID == target.Operation && api.Effect == "read" && strings.ToUpper(api.Method) == probe.Method {
					return "operation", matchesDeliveryOperationPath(api.Path, actual.EscapedPath())
				}
			}
		}
	}
	return "", false
}

func matchesDeliveryOperationPath(pattern, actual string) bool {
	var expression strings.Builder
	previous := 0
	for _, match := range deliveryPathParameter.FindAllStringIndex(pattern, -1) {
		expression.WriteString(regexp.QuoteMeta(pattern[previous:match[0]]))
		expression.WriteString("[^/]+")
		previous = match[1]
	}
	expression.WriteString(regexp.QuoteMeta(pattern[previous:]))
	matched, err := regexp.MatchString("^"+expression.String()+"$", actual)
	return err == nil && matched
}

// The signature attests receipt of a response from this candidate and operation.
// The selected AI determines whether that response meets the business goal.
func deliveryProbeProof(key string, attempt model.AppDeploymentVersion, kind, target string, status int) string {
	if key == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = fmt.Fprintf(mac, "opc-runtime-probe-v1\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		attempt.ID, attempt.DeployedImageName, attempt.DeliveryPlanSHA256, kind, target, status)
	return hex.EncodeToString(mac.Sum(nil))
}

func decodeDeliveryProbeContract(data []byte) (deliveryProbeContract, error) {
	var contract deliveryProbeContract
	err := json.Unmarshal(data, &contract)
	return contract, err
}

package middlewares

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// TODO: NT: ask response format
type secretNameResponse struct {
	SecretNames []secretName `json:"secrets"`
}

type secretName struct {
	Name string `json:"name"`
}

type secretResponse struct {
	SecretData secretData `json:"data"`
}

type secretData struct {
	Token string `json:"token"` // Base64 Encoded
}

type secretLocator struct {
	client *http.Client
}

func CreateSecretLocator(client *http.Client) SecretLocator {
	return &secretLocator{client: client}
}

func (s *secretLocator) GetName(clusterUrl, clusterToken, nsName, nsType string) (string, error) {
	// https://api.starter-us-east-2a.openshift.com/api/v1/namespaces/nvirani-preview-che/serviceaccounts/che
	url := fmt.Sprintf("%sapi/v1/namespaces/%s/serviceaccounts/%s", clusterUrl, nsName, nsType)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(Authorization, "Bearer "+clusterToken)
	resp, err := s.client.Do(req)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Call to '%s' failed with status '%s'", url, resp.Status)
	}
	defer resp.Body.Close()

	var r secretNameResponse
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return "", err
	}
	return getSecretName(r)
}

func (s *secretLocator) GetSecret(clusterUrl, clusterToken, nsName, secretName string) (string, error) {
	// https://api.starter-us-east-2a.openshift.com/api/v1/namespaces/nvirani-preview-che/secrets/che-token-w6h6f
	url := fmt.Sprintf("%sapi/v1/namespaces/%s/secrets/%s", clusterUrl, nsName, secretName)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(Authorization, "Bearer "+clusterToken)
	resp, err := s.client.Do(req)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Call to '%s' failed with status '%s'", url, resp.Status)
	}
	defer resp.Body.Close()

	var r secretResponse
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return "", err
	}
	return getSecret(r)
}

func getSecretName(resp secretNameResponse) (string, error) {
	for _, n := range resp.SecretNames {
		if strings.HasPrefix(n.Name, "che-token") {
			return n.Name, nil
		}
	}
	return "", errors.New("unable to locate secret name")
}

func getSecret(resp secretResponse) (string, error) {
	if resp.SecretData.Token == "" {
		return "", errors.New("unable to locate secret")
	}
	b, err := base64.StdEncoding.DecodeString(resp.SecretData.Token)
	return string(b), err
}

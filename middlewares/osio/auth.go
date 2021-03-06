package middlewares

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/containous/traefik/log"
)

const (
	Authorization = "Authorization"
	UserIDHeader  = "Impersonate-User"
	UserIDParam   = "identity_id"
)

const (
	// service maps to token type, if not a service token then it maps to UserToken
	CheToken  TokenType = "che"
	UserToken TokenType = "user"
)

var TokenTypeMap = map[string]TokenType{
	"rh-che": CheToken,
}

type TokenType string

type TenantLocator interface {
	GetTenant(token string, tokenType TokenType) (namespace, error)
	GetTenantById(token string, tokenType TokenType, userID string) (namespace, error)
}

type TenantTokenLocator interface {
	GetTokenWithUserToken(userToken, location string) (string, error)
	GetTokenWithSAToken(saToken, location string) (string, error)
}

type SrvAccTokenLocator func() (string, error)

type SecretLocator interface {
	GetName(clusterUrl, clusterToken, nsName, nsType string) (string, error)
	GetSecret(clusterUrl, clusterToken, nsName, secretName string) (string, error)
}

type TokenTypeLocator func(string) (TokenType, error)

type cacheData struct {
	Token    string
	Location string
}

type OSIOAuth struct {
	RequestTenantLocation TenantLocator
	RequestTenantToken    TenantTokenLocator
	RequestSrvAccToken    SrvAccTokenLocator
	RequestSecretLocation SecretLocator
	RequestTokenType      TokenTypeLocator
	cache                 *Cache
}

func NewPreConfiguredOSIOAuth() *OSIOAuth {
	authTokenKey := os.Getenv("AUTH_TOKEN_KEY")
	if authTokenKey == "" {
		panic("Missing AUTH_TOKEN_KEY")
	}
	tenantURL := os.Getenv("TENANT_URL")
	if tenantURL == "" {
		panic("Missing TENANT_URL")
	}
	authURL := os.Getenv("AUTH_URL")
	if authURL == "" {
		panic("Missing AUTH_URL")
	}

	srvAccID := os.Getenv("SERVICE_ACCOUNT_ID")
	if len(srvAccID) <= 0 {
		panic("Missing SERVICE_ACCOUNT_ID")
	}
	srvAccSecret := os.Getenv("SERVICE_ACCOUNT_SECRET")
	if len(srvAccSecret) <= 0 {
		panic("Missing SERVICE_ACCOUNT_SECRET")
	}
	return NewOSIOAuth(tenantURL, authURL, srvAccID, srvAccSecret)
}

func NewOSIOAuth(tenantURL, authURL, srvAccID, srvAccSecret string) *OSIOAuth {
	return &OSIOAuth{
		RequestTenantLocation: CreateTenantLocator(http.DefaultClient, tenantURL),
		RequestTenantToken:    CreateTenantTokenLocator(http.DefaultClient, authURL),
		RequestSrvAccToken:    CreateSrvAccTokenLocator(authURL, srvAccID, srvAccSecret),
		RequestSecretLocation: CreateSecretLocator(http.DefaultClient),
		RequestTokenType:      CreateTokenTypeLocator(http.DefaultClient, authURL),
		cache:                 &Cache{},
	}
}

func cacheResolverByID(tenantLocator TenantLocator, tokenLocator TenantTokenLocator, srvAccTokenLocator SrvAccTokenLocator, secretLocator SecretLocator, token string, tokenType TokenType, userID string) Resolver {
	return func() (interface{}, error) {
		ns, err := tenantLocator.GetTenantById(token, tokenType, userID)
		if err != nil {
			log.Errorf("Failed to locate tenant, %v", err)
			return cacheData{}, err
		}
		loc := ns.ClusterURL
		osoProxySAToken, err := srvAccTokenLocator()
		if err != nil {
			log.Errorf("Failed to locate service account token, %v", err)
			return cacheData{}, err
		}
		clusterToken, err := tokenLocator.GetTokenWithSAToken(osoProxySAToken, loc)
		if err != nil {
			log.Errorf("Failed to locate cluster token, %v", err)
			return cacheData{}, err
		}
		secretName, err := secretLocator.GetName(ns.ClusterURL, clusterToken, ns.Name, ns.Type)
		if err != nil {
			log.Errorf("Failed to locate secret name, %v", err)
			return cacheData{}, err
		}
		osoToken, err := secretLocator.GetSecret(ns.ClusterURL, clusterToken, ns.Name, secretName)
		if err != nil {
			log.Errorf("Failed to get secret, %v", err)
			return cacheData{}, err
		}
		return cacheData{Location: loc, Token: osoToken}, nil
	}
}

func cacheResolverByToken(tenantLocator TenantLocator, tokenLocator TenantTokenLocator, token string, tokenType TokenType) Resolver {
	return func() (interface{}, error) {
		ns, err := tenantLocator.GetTenant(token, tokenType)
		if err != nil {
			log.Errorf("Failed to locate tenant, %v", err)
			return cacheData{}, err
		}
		loc := ns.ClusterURL
		osoToken, err := tokenLocator.GetTokenWithUserToken(token, loc)
		if err != nil {
			log.Errorf("Failed to locate token, %v", err)
			return cacheData{}, err
		}
		return cacheData{Location: loc, Token: osoToken}, nil
	}
}

func (a *OSIOAuth) resolveByToken(token string, tokenType TokenType) (cacheData, error) {
	key := cacheKey(token)
	val, err := a.cache.Get(key, cacheResolverByToken(a.RequestTenantLocation, a.RequestTenantToken, token, tokenType)).Get()

	if data, ok := val.(cacheData); ok {
		return data, err
	}
	return cacheData{}, err
}

func (a *OSIOAuth) resolveByID(userID, token string, tokenType TokenType) (cacheData, error) {
	plainKey := fmt.Sprintf("%s_%s", token, userID)
	key := cacheKey(plainKey)
	val, err := a.cache.Get(key, cacheResolverByID(a.RequestTenantLocation, a.RequestTenantToken, a.RequestSrvAccToken, a.RequestSecretLocation, token, tokenType, userID)).Get()

	if data, ok := val.(cacheData); ok {
		return data, err
	}
	return cacheData{}, err
}

func (a *OSIOAuth) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {

	if a.RequestTenantLocation != nil {

		if r.Method != "OPTIONS" {
			token, err := getToken(r)
			if err != nil {
				log.Errorf("Token not found, %v", err)
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}

			tokenType, err := a.RequestTokenType(token)
			if err != nil {
				log.Errorf("Invalid token, %v", err)
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}

			var cached cacheData
			if tokenType != UserToken {
				userID := extractUserID(r)
				if userID == "" {
					log.Errorf("user identity is missing")
					rw.WriteHeader(http.StatusUnauthorized)
					return
				}
				cached, err = a.resolveByID(userID, token, tokenType)
			} else {
				cached, err = a.resolveByToken(token, tokenType)
			}

			if err != nil {
				log.Errorf("Cache resole failed, %v", err)
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}

			r.Header.Set("Target", cached.Location)
			r.Header.Set("Authorization", "Bearer "+cached.Token)
			if tokenType != UserToken {
				removeUserID(r)
			}
		} else {
			r.Header.Set("Target", "default")
		}
	}
	next(rw, r)
}

func getToken(r *http.Request) (string, error) {
	if at := r.URL.Query().Get("access_token"); at != "" {
		r.URL.Query().Del("access_token")
		return at, nil
	}
	t, err := extractToken(r.Header.Get(Authorization))
	if err != nil {
		return "", err
	}
	if t == "" {
		return "", fmt.Errorf("Missing auth")
	}
	return t, nil
}

func extractToken(auth string) (string, error) {
	auths := strings.Split(auth, " ")
	if len(auths) == 0 {
		return "", fmt.Errorf("Invalid auth")
	}
	return auths[len(auths)-1], nil
}

func cacheKey(plainKey string) string {
	h := sha256.New()
	h.Write([]byte(plainKey))
	hash := hex.EncodeToString(h.Sum(nil))
	return hash
}

func extractUserID(req *http.Request) string {
	userID := ""
	if req.Header.Get(UserIDHeader) != "" {
		userID = req.Header.Get(UserIDHeader)
	} else if req.URL.Query().Get(UserIDParam) != "" {
		userID = req.URL.Query().Get(UserIDParam)
		if strings.Contains(userID, "/") {
			endInd := strings.Index(userID, "/")
			userID = userID[:endInd]
		}
	}
	return userID
}

func removeUserID(req *http.Request) {
	if req.Header.Get(UserIDHeader) != "" {
		req.Header.Del(UserIDHeader)
	}
	if req.URL.Query().Get(UserIDParam) != "" {
		userID := req.URL.Query().Get(UserIDParam)
		if strings.Contains(userID, "/") {
			q := req.URL.Query()
			q.Del(UserIDParam)
			req.URL.RawQuery = q.Encode()
			startInd := strings.Index(userID, "/")
			req.URL.Path = userID[startInd:]
			req.RequestURI = req.URL.RequestURI()
		} else {
			q := req.URL.Query()
			q.Del(UserIDParam)
			req.URL.RawQuery = q.Encode()
		}
	}
}

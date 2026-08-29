package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Identity is who GitHub says approved the registry.
//
// GitHubID is the identity; Login is a display name that its owner may change and that
// somebody else may then take.
type Identity struct {
	GitHubID  int64
	Login     string
	AvatarURL string
}

// Provider is the sign-in half of authentication, kept behind an interface so the flow can
// be exercised without GitHub.
type Provider interface {
	AuthorizeURL(state string) string
	Identify(ctx context.Context, code string) (Identity, error)
}

// GitHub signs a user in through GitHub's OAuth app flow.
type GitHub struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string

	AuthorizeEndpoint string
	TokenEndpoint     string
	UserEndpoint      string

	Client *http.Client
}

func NewGitHub(clientID, clientSecret, redirectURI string) *GitHub {
	return &GitHub{
		ClientID:          clientID,
		ClientSecret:      clientSecret,
		RedirectURI:       redirectURI,
		AuthorizeEndpoint: "https://github.com/login/oauth/authorize",
		TokenEndpoint:     "https://github.com/login/oauth/access_token",
		UserEndpoint:      "https://api.github.com/user",
		Client:            &http.Client{Timeout: 15 * time.Second},
	}
}

// AuthorizeURL is where a browser is sent to approve the registry. It asks for read:user
// and nothing more: the registry reads a public identity and never acts on anyone's behalf.
func (g *GitHub) AuthorizeURL(state string) string {
	query := url.Values{
		"client_id":    {g.ClientID},
		"redirect_uri": {g.RedirectURI},
		"scope":        {"read:user"},
		"state":        {state},
	}

	return g.AuthorizeEndpoint + "?" + query.Encode()
}

// Identify exchanges a callback's code for the identity that approved it.
func (g *GitHub) Identify(ctx context.Context, code string) (Identity, error) {
	token, err := g.exchange(ctx, code)
	if err != nil {
		return Identity{}, err
	}

	return g.identity(ctx, token)
}

func (g *GitHub) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"redirect_uri":  {g.RedirectURI},
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("auth: building the token request: %w", err)
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	var exchanged struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}

	if err := g.call(request, &exchanged); err != nil {
		return "", err
	}

	// GitHub answers 200 with an error body rather than a status, so the body is the
	// only place a refusal shows up
	if exchanged.Error != "" {
		return "", fmt.Errorf("auth: GitHub refused the sign-in: %s", describe(exchanged.Error, exchanged.ErrorDescription))
	}

	if exchanged.AccessToken == "" {
		return "", errors.New("auth: GitHub returned no access token")
	}

	return exchanged.AccessToken, nil
}

func (g *GitHub) identity(ctx context.Context, token string) (Identity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.UserEndpoint, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: building the identity request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")

	var read struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}

	if err := g.call(request, &read); err != nil {
		return Identity{}, err
	}

	if read.ID == 0 || read.Login == "" {
		return Identity{}, errors.New("auth: GitHub returned no usable identity")
	}

	return Identity{GitHubID: read.ID, Login: read.Login, AvatarURL: read.AvatarURL}, nil
}

// call sends a request and decodes its JSON, bounding what it will read: an endpoint
// answering with something enormous is not a reason to hold it all.
func (g *GitHub) call(request *http.Request, into any) error {
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("auth: reaching %s: %w", request.URL.Host, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("auth: reading from %s: %w", request.URL.Host, err)
	}

	if response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("auth: %s answered %d", request.URL.Host, response.StatusCode)
	}

	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("auth: %s answered with something other than JSON", request.URL.Host)
	}

	return nil
}

func describe(code, description string) string {
	if description == "" {
		return code
	}

	return description
}

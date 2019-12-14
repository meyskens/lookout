package github

import (
	"context"
	"net/http"
	"time"

	"github.com/meyskens/lookout/util/cache"
	"github.com/meyskens/lookout/util/ctxlog"

	"github.com/gregjones/httpcache"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	githttp "gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	log "gopkg.in/src-d/go-log.v1"
	"gopkg.in/meyskens/lookout-sdk.v0/pb"
)

// ClientConfig holds github username, token and watch interval
type ClientConfig struct {
	User        string
	Token       string
	MinInterval string
}

var zeroClientConfig = ClientConfig{}

// IsZero return true if config is empty and false otherwise
func (c ClientConfig) IsZero() bool {
	return c == zeroClientConfig
}

// NewClientPoolFromTokens creates new ClientPool based on map[repoURL]ClientConfig
// later we will need another constructor that would request installations and create pool from it
func NewClientPoolFromTokens(
	urlToConfig map[string]ClientConfig,
	cache *cache.ValidableCache,
	timeout time.Duration,
) (*ClientPool, error) {
	byConfig := make(map[ClientConfig][]*repositoryInfo)

	for url, c := range urlToConfig {
		repo, err := pb.ParseRepositoryInfo(url)
		if err != nil {
			return nil, err
		}

		// repositoryInfo.OrganizationID is left unset for clients using personal tokens
		byConfig[c] = append(byConfig[c], &repositoryInfo{RepositoryInfo: *repo})
	}

	byClients := make(map[*Client][]*repositoryInfo, len(byConfig))
	byRepo := make(map[string]*Client, len(urlToConfig))
	for conf, repos := range byConfig {
		cachedT := httpcache.NewTransport(cache)
		cachedT.MarkCachedResponses = true

		rt := &basicAuthRoundTripper{
			User:     conf.User,
			Password: conf.Token,
			Base:     cachedT,
		}

		// Auth must be: https://<token>@github.com/owner/repo.git
		// Reference: https://blog.github.com/2012-09-21-easier-builds-and-deployments-using-git-over-https-and-oauth/
		gitAuth := func(ctx context.Context) transport.AuthMethod {
			return &githttp.BasicAuth{
				Username: conf.Token,
				Password: "",
			}
		}

		client := NewClient(rt, cache, conf.MinInterval, gitAuth, timeout)
		if err := ValidateTokenPermissions(client); err != nil {
			return nil, err
		}

		if _, ok := byClients[client]; !ok {
			byClients[client] = []*repositoryInfo{}
		}

		byClients[client] = append(byClients[client], repos...)
		for _, r := range repos {
			err := CanPostStatus(client, r)
			if err != nil {
				return nil, err
			}

			byRepo[r.FullName] = client
		}
	}

	pool := newClientPoolFromClients(byClients, byRepo)
	return pool, nil
}

type basicAuthRoundTripper struct {
	Base     http.RoundTripper
	User     string
	Password string
}

func (t *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctxlog.Get(req.Context()).With(log.Fields{
		"url":  req.URL.String(),
		"user": t.User,
	}).Debugf("http request with GitHub auth")

	if t.User != "" {
		req.SetBasicAuth(t.User, t.Password)
	}

	rt := t.Base
	if rt == nil {
		rt = http.DefaultTransport
	}

	return rt.RoundTrip(req)
}

var _ http.RoundTripper = &basicAuthRoundTripper{}

/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flagutil

import (
	"bytes"
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"

	jwt "github.com/dgrijalva/jwt-go/v4"
	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/config/secret"
	"k8s.io/test-infra/prow/git"
	"k8s.io/test-infra/prow/github"
)

// GitHubOptions holds options for interacting with GitHub.
//
// Set AllowAnonymous to be true if you want to allow anonymous github access.
// Set AllowDirectAccess to be true if you want to suppress warnings on direct github access (without ghproxy).
type GitHubOptions struct {
	Host              string
	endpoint          Strings
	graphqlEndpoint   string
	TokenPath         string
	AllowAnonymous    bool
	AllowDirectAccess bool
	AppID             string
	AppPrivateKeyPath string

	ThrottleHourlyTokens int
	ThrottleAllowBurst   int

	// This will only be set after a github client was retrieved for the first time
	appsTokenGenerator github.GitHubAppTokenGenerator
}

// flagParams struct is used indirectly by users of this package to customize
// the common flags behavior, such as providing their own default values
// or suppressing presence of certain flags.
type flagParams struct {
	defaults GitHubOptions

	disableThrottlerOptions bool
}

const DefaultGitHubTokenPath = "/etc/github/oauth" // Exported for testing purposes

type FlagParameter func(options *flagParams)

// ThrottlerDefaults allows to customize the default values of flags
// that control the throttler behavior. Setting `hourlyTokens` to zero
// disables throttling by default.
func ThrottlerDefaults(hourlyTokens, allowedBursts int) FlagParameter {
	return func(o *flagParams) {
		o.defaults.ThrottleHourlyTokens = hourlyTokens
		o.defaults.ThrottleAllowBurst = allowedBursts
	}
}

// DisableThrottlerOptions suppresses the presence of throttler-related flags,
// effectively disallowing external users to parametrize default throttling
// behavior. This is useful mostly when a program creates multiple GH clients
// with different behavior.
func DisableThrottlerOptions() FlagParameter {
	return func(o *flagParams) {
		o.disableThrottlerOptions = true
	}
}

// AddCustomizedFlags injects GitHub options into the given FlagSet. Behavior can be customized
// via the functional options.
func (o *GitHubOptions) AddCustomizedFlags(fs *flag.FlagSet, paramFuncs ...FlagParameter) {
	o.addFlags(fs, paramFuncs...)
}

// AddFlags injects GitHub options into the given FlagSet
func (o *GitHubOptions) AddFlags(fs *flag.FlagSet) {
	o.addFlags(fs)
}

func (o *GitHubOptions) addFlags(fs *flag.FlagSet, paramFuncs ...FlagParameter) {
	params := flagParams{
		defaults: GitHubOptions{
			Host:            github.DefaultHost,
			endpoint:        NewStrings(github.DefaultAPIEndpoint),
			graphqlEndpoint: github.DefaultGraphQLEndpoint,
		},
	}

	for _, parametrize := range paramFuncs {
		parametrize(&params)
	}

	defaults := params.defaults
	fs.StringVar(&o.Host, "github-host", defaults.Host, "GitHub's default host (may differ for enterprise)")
	o.endpoint = NewStrings(defaults.endpoint.Strings()...)
	fs.Var(&o.endpoint, "github-endpoint", "GitHub's API endpoint (may differ for enterprise).")
	fs.StringVar(&o.graphqlEndpoint, "github-graphql-endpoint", defaults.graphqlEndpoint, "GitHub GraphQL API endpoint (may differ for enterprise).")
	fs.StringVar(&o.TokenPath, "github-token-path", defaults.TokenPath, "Path to the file containing the GitHub OAuth secret.")
	fs.StringVar(&o.AppID, "github-app-id", defaults.AppID, "ID of the GitHub app. If set, requires --github-app-private-key-path to be set and --github-token-path to be unset.")
	fs.StringVar(&o.AppPrivateKeyPath, "github-app-private-key-path", defaults.AppPrivateKeyPath, "Path to the private key of the github app. If set, requires --github-app-id to bet set and --github-token-path to be unset")

	if !params.disableThrottlerOptions {
		fs.IntVar(&o.ThrottleHourlyTokens, "github-hourly-tokens", defaults.ThrottleHourlyTokens, "If set to a value larger than zero, enable client-side throttling to limit hourly token consumption. If set, --github-allowed-burst must be positive too.")
		fs.IntVar(&o.ThrottleAllowBurst, "github-allowed-burst", defaults.ThrottleAllowBurst, "Size of token consumption bursts. If set, --github-hourly-tokens must be positive too and set to a higher or equal number.")
	}
}

// Validate validates GitHub options. Note that validate updates the GitHubOptions
// to add default values for TokenPath and graphqlEndpoint.
func (o *GitHubOptions) Validate(bool) error {
	endpoints := o.endpoint.Strings()
	for i, uri := range endpoints {
		if uri == "" {
			endpoints[i] = github.DefaultAPIEndpoint
		} else if _, err := url.ParseRequestURI(uri); err != nil {
			return fmt.Errorf("invalid -github-endpoint URI: %q", uri)
		}
	}

	if o.TokenPath != "" && (o.AppID != "" || o.AppPrivateKeyPath != "") {
		return fmt.Errorf("--token-path is mutually exclusive with --app-id and --app-private-key-path")
	}
	if o.AppID == "" != (o.AppPrivateKeyPath == "") {
		return errors.New("--app-id and --app-private-key-path must be set together")
	}

	if o.TokenPath != "" && len(endpoints) == 1 && endpoints[0] == github.DefaultAPIEndpoint && !o.AllowDirectAccess {
		logrus.Warn("It doesn't look like you are using ghproxy to cache API calls to GitHub! This has become a required component of Prow and other components will soon be allowed to add features that may rapidly consume API ratelimit without caching. Starting May 1, 2020 use Prow components without ghproxy at your own risk! https://github.com/kubernetes/test-infra/tree/master/ghproxy#ghproxy")
	}

	if o.graphqlEndpoint == "" {
		o.graphqlEndpoint = github.DefaultGraphQLEndpoint
	} else if _, err := url.Parse(o.graphqlEndpoint); err != nil {
		return fmt.Errorf("invalid -github-graphql-endpoint URI: %q", o.graphqlEndpoint)
	}

	if (o.ThrottleHourlyTokens > 0) != (o.ThrottleAllowBurst > 0) {
		if o.ThrottleHourlyTokens == 0 {
			// Tolerate `--github-hourly-tokens=0` alone to disable throttling
			o.ThrottleAllowBurst = 0
		} else {
			return errors.New("--github-hourly-tokens and --github-allowed-burst must be either both higher than zero or both equal to zero")
		}
	}
	if o.ThrottleAllowBurst > o.ThrottleHourlyTokens {
		return errors.New("--github-allowed-burst must not be larger than --github-hourly-tokens")
	}

	return nil
}

// GitHubClientWithLogFields returns a GitHub client with extra logging fields
func (o *GitHubOptions) GitHubClientWithLogFields(secretAgent *secret.Agent, dryRun bool, fields logrus.Fields) (github.Client, error) {
	client, err := o.githubClient(secretAgent, dryRun)
	if err != nil {
		return nil, err
	}
	return client.WithFields(fields), nil
}

func (o *GitHubOptions) githubClient(secretAgent *secret.Agent, dryRun bool) (github.Client, error) {
	fields := logrus.Fields{}
	var generator *func() []byte
	if o.TokenPath == "" {
		logrus.Warn("empty -github-token-path, will use anonymous github client")
		generatorFunc := func() []byte {
			return []byte{}
		}
		generator = &generatorFunc
	} else {
		if secretAgent == nil {
			return nil, fmt.Errorf("cannot store token from %q without a secret agent", o.TokenPath)
		}
		if err := secretAgent.Add(o.TokenPath); err != nil {
			return nil, fmt.Errorf("failed to add GitHub token to secret agent: %w", err)
		}
		generatorFunc := secretAgent.GetTokenGenerator(o.TokenPath)
		generator = &generatorFunc
	}

	var appsGenerator func() *rsa.PrivateKey
	if o.AppPrivateKeyPath != "" {
		if secretAgent == nil {
			return nil, fmt.Errorf("cannot store token from %q without a secret agent", o.AppPrivateKeyPath)
		}
		if err := secretAgent.Add(o.AppPrivateKeyPath); err != nil {
			return nil, fmt.Errorf("failed to add the the key from --app-private-key-path to secret agent: %w", err)
		}
		appsGenerator = func() *rsa.PrivateKey {
			raw := secretAgent.GetTokenGenerator(o.AppPrivateKeyPath)()
			privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(raw)
			// TODO alvaroaleman: Add hooks to the SecretAgent
			if err != nil {
				panic(fmt.Sprintf("failed to parse private key: %v", err))
			}
			return privateKey
		}

	}

	optionallyThrottled := func(c github.Client) github.Client {
		// Throttle handles zeros as "disable throttling" so we do not need to call it conditionally
		c.Throttle(o.ThrottleHourlyTokens, o.ThrottleHourlyTokens)
		return c
	}

	if dryRun {
		if o.AppPrivateKeyPath != "" {
			appsTokenGenerator, client := github.NewAppsAuthDryRunClientWithFields(fields, secretAgent.Censor, o.AppID, appsGenerator, o.graphqlEndpoint, o.endpoint.Strings()...)
			o.appsTokenGenerator = appsTokenGenerator
			return optionallyThrottled(client), nil
		}
		client := github.NewDryRunClientWithFields(fields, *generator, secretAgent.Censor, o.graphqlEndpoint, o.endpoint.Strings()...)
		return optionallyThrottled((client)), nil
	}
	if o.AppPrivateKeyPath != "" {
		appsTokenGenerator, client := github.NewAppsAuthClientWithFields(fields, secretAgent.Censor, o.AppID, appsGenerator, o.graphqlEndpoint, o.endpoint.Strings()...)
		o.appsTokenGenerator = appsTokenGenerator
		return optionallyThrottled(client), nil
	}

	return optionallyThrottled(github.NewClientWithFields(fields, *generator, secretAgent.Censor, o.graphqlEndpoint, o.endpoint.Strings()...)), nil
}

// GitHubClient returns a GitHub client.
func (o *GitHubOptions) GitHubClient(secretAgent *secret.Agent, dryRun bool) (github.Client, error) {
	return o.GitHubClientWithLogFields(secretAgent, dryRun, logrus.Fields{})
}

// GitHubClientWithAccessToken creates a GitHub client from an access token.
func (o *GitHubOptions) GitHubClientWithAccessToken(token string) github.Client {
	return github.NewClient(func() []byte { return []byte(token) }, func(content []byte) []byte {
		trimmedToken := strings.TrimSpace(token)
		if trimmedToken != token {
			token = trimmedToken
		}
		if token == "" {
			return content
		}
		return bytes.ReplaceAll(content, []byte(token), []byte("CENSORED"))
	}, o.graphqlEndpoint, o.endpoint.Strings()...)
}

// GitClient returns a Git client.
func (o *GitHubOptions) GitClient(secretAgent *secret.Agent, dryRun bool) (client *git.Client, err error) {
	client, err = git.NewClientWithHost(o.Host)
	if err != nil {
		return nil, err
	}

	// We must capture the value of client here to prevent issues related
	// to the use of named return values when an error is encountered.
	// Without this, we risk a nil pointer dereference.
	defer func(client *git.Client) {
		if err != nil {
			client.Clean()
		}
	}(client)

	user, generator, err := o.getGitAuthentication(secretAgent, dryRun)
	if err != nil {
		return nil, fmt.Errorf("failed to get git authentication: %w", err)
	}
	client.SetCredentials(user, generator)

	return client, nil
}

func (o *GitHubOptions) getGitAuthentication(secretAgent *secret.Agent, dryRun bool) (string, git.GitTokenGenerator, error) {
	githubClient, err := o.GitHubClient(secretAgent, dryRun)
	if err != nil {
		return "", nil, fmt.Errorf("error getting GitHub client: %v", err)
	}

	// Use Personal Access token auth
	if o.appsTokenGenerator == nil {
		botUser, err := githubClient.BotUser()
		if err != nil {
			return "", nil, fmt.Errorf("error getting bot name: %v", err)
		}
		generator := func(_ string) (string, error) {
			return string(secretAgent.GetTokenGenerator(o.TokenPath)()), nil
		}

		return botUser.Login, generator, nil
	}

	// Use github apps auth
	// https://docs.github.com/en/free-pro-team@latest/developers/apps/authenticating-with-github-apps#http-based-git-access-by-an-installation
	return "x-access-token", git.GitTokenGenerator(o.appsTokenGenerator), nil
}

package creds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/docker"

	"github.com/containers/image/v5/pkg/docker/config"
	containersTypes "github.com/containers/image/v5/types"
	"github.com/docker/docker-credential-helpers/client"
	"github.com/docker/docker-credential-helpers/credentials"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

type CredentialsCallback func(registry string) (docker.Credentials, error)

var ErrUnauthorized = errors.New("bad credentials")

// VerifyCredentialsCallback checks if credentials are accepted by the registry.
// If credentials are incorrect this callback shall return ErrUnauthorized.
type VerifyCredentialsCallback func(ctx context.Context, registry string, credentials docker.Credentials) error

// CheckAuth verifies that credentials are correct
func CheckAuth(ctx context.Context, registry string, credentials docker.Credentials, trans http.RoundTripper) error {
	authenticator := &authn.Basic{
		Username: credentials.Username,
		Password: credentials.Password,
	}

	reg, err := name.NewRegistry(registry)
	if err != nil {
		return err
	}

	serverAddress := registry
	if !strings.HasPrefix(serverAddress, "https://") && !strings.HasPrefix(serverAddress, "http://") {
		serverAddress = reg.Scheme() + "://" + serverAddress
	}

	url := fmt.Sprintf("%s/v2/", serverAddress)

	tr, err := transport.NewWithContext(ctx, reg, authenticator, trans, nil)
	if err != nil {
		var transportErr *transport.Error
		if errors.As(err, &transportErr) && transportErr.StatusCode == 401 {
			return ErrUnauthorized
		}
		return err
	}

	cli := http.Client{Transport: tr}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("failed to verify credentials: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("failed to verify credentials: status code: %d", resp.StatusCode)
	default:
		return nil
	}
}

type ChooseCredentialHelperCallback func(available []string) (string, error)

type credentialsProvider struct {
	promptForCredentials     CredentialsCallback
	verifyCredentials        VerifyCredentialsCallback
	promptForCredentialStore ChooseCredentialHelperCallback
	credentialLoaders        []CredentialsCallback
	authFilePath             string
	transport                http.RoundTripper
}

type Opt func(opts *credentialsProvider)

// WithPromptForCredentials sets custom callback that is supposed to
// interactively ask for credentials in case the credentials cannot be found in configuration files.
// The callback may be called multiple times in case incorrect credentials were returned before.
func WithPromptForCredentials(cbk CredentialsCallback) Opt {
	return func(opts *credentialsProvider) {
		opts.promptForCredentials = cbk
	}
}

// WithVerifyCredentials sets custom callback for credentials validation.
func WithVerifyCredentials(cbk VerifyCredentialsCallback) Opt {
	return func(opts *credentialsProvider) {
		opts.verifyCredentials = cbk
	}
}

// WithPromptForCredentialStore sets custom callback that is supposed to
// interactively ask user which credentials store/helper is used to store credentials obtained
// from user.
func WithPromptForCredentialStore(cbk ChooseCredentialHelperCallback) Opt {
	return func(opts *credentialsProvider) {
		opts.promptForCredentialStore = cbk
	}
}

func WithTransport(transport http.RoundTripper) Opt {
	return func(opts *credentialsProvider) {
		opts.transport = transport
	}
}

// WithAdditionalCredentialLoaders adds custom callbacks for credential retrieval.
// The callbacks are supposed to be non-interactive as opposed to WithPromptForCredentials.
//
// This might be useful when credentials are shared with some other service.
//
// Example: OpenShift builtin registry shares credentials with the cluster (k8s) credentials.
func WithAdditionalCredentialLoaders(loaders ...CredentialsCallback) Opt {
	return func(opts *credentialsProvider) {
		opts.credentialLoaders = append(opts.credentialLoaders, loaders...)
	}
}

// NewCredentialsProvider returns new CredentialsProvider that tries to get credentials from docker/func config files.
//
// In case getting credentials from the config files fails
// the caller provided callback (see WithPromptForCredentials) will be invoked to obtain credentials.
// The callback may be called multiple times in case the returned credentials
// are not correct (see WithVerifyCredentials).
//
// When the callback succeeds the credentials will be saved by using helper defined in the func config.
// If the helper is not defined in the config file
// it may be picked by provided callback (see WithPromptForCredentialStore).
// The picked value will be saved in the func config.
//
// To verify that credentials are correct custom callback can be used (see WithVerifyCredentials).
func NewCredentialsProvider(opts ...Opt) docker.CredentialsProvider {
	var c credentialsProvider

	for _, o := range opts {
		o(&c)
	}

	if c.transport == nil {
		c.transport = http.DefaultTransport
	}

	if c.verifyCredentials == nil {
		c.verifyCredentials = func(ctx context.Context, registry string, credentials docker.Credentials) error {
			return CheckAuth(ctx, registry, credentials, c.transport)
		}
	}

	if c.promptForCredentialStore == nil {
		c.promptForCredentialStore = func(available []string) (string, error) {
			return "", nil
		}
	}

	c.authFilePath = filepath.Join(fn.ConfigPath(), "auth.json")
	sys := &containersTypes.SystemContext{
		AuthFilePath: c.authFilePath,
	}

	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	dockerConfigPath := filepath.Join(home, ".docker", "config.json")

	var defaultCredentialLoaders = []CredentialsCallback{
		func(registry string) (docker.Credentials, error) { // empty credentials provider for unsecured registries
			return docker.Credentials{}, nil
		},
		func(registry string) (docker.Credentials, error) {
			creds, err := config.GetCredentials(sys, registry)
			if err != nil {
				return docker.Credentials{}, err
			}
			return docker.Credentials{
				Username: creds.Username,
				Password: creds.Password,
			}, nil
		},
		func(registry string) (docker.Credentials, error) {
			return getCredentialsByCredentialHelper(c.authFilePath, registry)
		},
		func(registry string) (docker.Credentials, error) {
			return getCredentialsByCredentialHelper(dockerConfigPath, registry)
		},
	}

	c.credentialLoaders = append(c.credentialLoaders, defaultCredentialLoaders...)

	return c.getCredentials
}

func (c *credentialsProvider) getCredentials(ctx context.Context, registry string) (docker.Credentials, error) {
	var err error
	result := docker.Credentials{}

	for _, load := range c.credentialLoaders {

		result, err = load(registry)

		if err != nil {
			if errors.Is(err, errCredentialsNotFound) {
				continue
			}
			return docker.Credentials{}, err
		}

		err = c.verifyCredentials(ctx, registry, result)
		if err == nil {
			return result, nil
		} else {
			if !errors.Is(err, ErrUnauthorized) {
				return docker.Credentials{}, err
			}
		}

	}

	if c.promptForCredentials == nil {
		return docker.Credentials{}, errCredentialsNotFound
	}

	for {
		result, err = c.promptForCredentials(registry)
		if err != nil {
			return docker.Credentials{}, err
		}

		err = c.verifyCredentials(ctx, registry, result)
		if err == nil {
			err = setCredentialsByCredentialHelper(c.authFilePath, registry, result.Username, result.Password)
			if err != nil {
				if !errors.Is(err, errNoCredentialHelperConfigured) {
					return docker.Credentials{}, err
				}
				helpers := listCredentialHelpers()
				helper, err := c.promptForCredentialStore(helpers)
				if err != nil {
					return docker.Credentials{}, err
				}
				helper = strings.TrimPrefix(helper, "docker-credential-")
				err = setCredentialHelperToConfig(c.authFilePath, helper)
				if err != nil {
					return docker.Credentials{}, fmt.Errorf("faild to set the helper to the config: %w", err)
				}
				err = setCredentialsByCredentialHelper(c.authFilePath, registry, result.Username, result.Password)
				if err != nil && !errors.Is(err, errNoCredentialHelperConfigured) {
					return docker.Credentials{}, err
				}
			}
			return result, nil
		} else {
			if errors.Is(err, ErrUnauthorized) {
				continue
			}
			return docker.Credentials{}, err
		}
	}
}

var errCredentialsNotFound = errors.New("credentials not found")
var errNoCredentialHelperConfigured = errors.New("no credential helper configure")

func getCredentialHelperFromConfig(confFilePath string) (string, error) {
	data, err := ioutil.ReadFile(confFilePath)
	if err != nil {
		return "", err
	}

	conf := struct {
		Store string `json:"credsStore"`
	}{}

	err = json.Unmarshal(data, &conf)
	if err != nil {
		return "", err
	}

	return conf.Store, nil
}

func setCredentialHelperToConfig(confFilePath, helper string) error {
	var err error

	configData := make(map[string]interface{})

	if data, err := ioutil.ReadFile(confFilePath); err == nil {
		err = json.Unmarshal(data, &configData)
		if err != nil {
			return err
		}
	}

	configData["credsStore"] = helper

	data, err := json.MarshalIndent(&configData, "", "    ")
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(confFilePath, data, 0600)
	if err != nil {
		return err
	}

	return nil
}

func getCredentialsByCredentialHelper(confFilePath, registry string) (docker.Credentials, error) {
	result := docker.Credentials{}

	helper, err := getCredentialHelperFromConfig(confFilePath)
	if err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("failed to get helper from config: %w", err)
	}
	if helper == "" {
		return result, errCredentialsNotFound
	}

	helperName := fmt.Sprintf("docker-credential-%s", helper)
	p := client.NewShellProgramFunc(helperName)

	credentialsMap, err := client.List(p)
	if err != nil {
		return result, fmt.Errorf("failed to list credentials: %w", err)
	}

	for serverUrl := range credentialsMap {
		if RegistryEquals(serverUrl, registry) {
			creds, err := client.Get(p, serverUrl)
			if err != nil {
				return result, fmt.Errorf("failed to get credentials: %w", err)
			}
			result.Username = creds.Username
			result.Password = creds.Secret
			return result, nil
		}
	}

	return result, fmt.Errorf("failed to get credentials from helper specified in ~/.docker/config.json: %w", errCredentialsNotFound)
}

func setCredentialsByCredentialHelper(confFilePath, registry, username, secret string) error {
	helper, err := getCredentialHelperFromConfig(confFilePath)

	if helper == "" || os.IsNotExist(err) {
		return errNoCredentialHelperConfigured
	}
	if err != nil {
		return fmt.Errorf("failed to get helper from config: %w", err)
	}

	helperName := fmt.Sprintf("docker-credential-%s", helper)
	p := client.NewShellProgramFunc(helperName)

	return client.Store(p, &credentials.Credentials{ServerURL: registry, Username: username, Secret: secret})
}

func listCredentialHelpers() []string {
	path := os.Getenv("PATH")
	paths := strings.Split(path, string(os.PathListSeparator))

	helpers := make(map[string]bool)
	for _, p := range paths {
		fss, err := ioutil.ReadDir(p)
		if err != nil {
			continue
		}
		for _, fi := range fss {
			if fi.IsDir() {
				continue
			}
			if !strings.HasPrefix(fi.Name(), "docker-credential-") {
				continue
			}
			if runtime.GOOS == "windows" {
				ext := filepath.Ext(fi.Name())
				if ext != ".exe" && ext != ".bat" {
					continue
				}
			}
			helpers[fi.Name()] = true
		}
	}
	result := make([]string, 0, len(helpers))
	for h := range helpers {
		result = append(result, h)
	}
	return result
}

func hostPort(registry string) (host string, port string) {
	host, port = registry, ""
	if !strings.Contains(registry, "://") {
		h, p, err := net.SplitHostPort(registry)

		if err == nil {
			host, port = h, p
			return
		}
		registry = "https://" + registry
	}

	u, err := url.Parse(registry)
	if err != nil {
		panic(err)
	}
	host = u.Hostname()
	port = u.Port()
	return
}

// RegistryEquals checks whether registry matches in host and port
// with exception where empty port matches standard ports (80,443)
func RegistryEquals(regA, regB string) bool {
	h1, p1 := hostPort(regA)
	h2, p2 := hostPort(regB)

	isStdPort := func(p string) bool { return p == "443" || p == "80" }

	portEq := p1 == p2 ||
		(p1 == "" && isStdPort(p2)) ||
		(isStdPort(p1) && p2 == "")

	if h1 == h2 && portEq {
		return true
	}

	if strings.HasSuffix(h1, "docker.io") &&
		strings.HasSuffix(h2, "docker.io") {
		return true
	}

	return false
}

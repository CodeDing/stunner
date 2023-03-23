package stunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pion/transport/v2"
	"sigs.k8s.io/yaml"

	"github.com/l7mp/stunner/internal/resolver"
	"github.com/l7mp/stunner/internal/util"
	"github.com/l7mp/stunner/pkg/apis/v1alpha1"
)

const confUpdatePeriod = 1 * time.Second

// Options defines various options for the STUNner server.
type Options struct {
	// DryRun suppresses sideeffects: STUNner will not initialize listener sockets and bring up
	// the TURN server, and it will not fire up the health-check and the metrics
	// servers. Intended for testing, default is false.
	DryRun bool
	// SuppressRollback controls whether to rollback to the last working configuration after a
	// failed reconciliation request. Default is false, which means to always do a rollback.
	SuppressRollback bool
	// LogLevel specifies the required loglevel for STUNner and each of its sub-objects, e.g.,
	// "all:TRACE" will force maximal loglevel throughout, "all:ERROR,auth:TRACE,turn:DEBUG"
	// will suppress all logs except in the authentication subsystem and the TURN protocol
	// logic.
	LogLevel string
	// Resolver swaps the internal DNS resolver with a custom implementation. Intended for
	// testing.
	Resolver resolver.DnsResolver
	// UDPListenerThreadNum determines the number of readloop threads spawned per UDP listener
	// (default is 4, must be >0 integer). TURN allocations will be automatically load-balanced
	// by the kernel UDP stack based on the client 5-tuple. This setting controls the maximum
	// number of CPU cores UDP listeners can scale to. Note that all other listener protocol
	// types (TCP, TLS and DTLS) use per-client threads, so this setting affects only UDP
	// listeners. For more info see https://github.com/pion/turn/pull/295.
	UDPListenerThreadNum int
	// VNet will switch on testing mode, using a vnet.Net instance to run STUNner over an
	// emulated data-plane.
	Net transport.Net
}

// NewDefaultStunnerConfig builds a default configuration from a TURN server URI. Example: the URI
// `turn://user:pass@127.0.0.1:3478?transport=udp` will be parsed into a STUNner configuration with
// a server running on the localhost at UDP port 3478, with plain-text authentication using the
// username/password pair `user:pass`. Health-checks and metric scarping are disabled.
func NewDefaultConfig(uri string) (*v1alpha1.StunnerConfig, error) {
	u, err := ParseUri(uri)
	if err != nil {
		return nil, fmt.Errorf("Invalid URI '%s': %s", uri, err)
	}

	if u.Username == "" || u.Password == "" {
		return nil, fmt.Errorf("Username/password must be set: '%s'", uri)
	}

	c := &v1alpha1.StunnerConfig{
		ApiVersion: v1alpha1.ApiVersion,
		Admin: v1alpha1.AdminConfig{
			LogLevel:        v1alpha1.DefaultLogLevel,
			MetricsEndpoint: "http://:8088",
		},
		Auth: v1alpha1.AuthConfig{
			Type:  "plaintext",
			Realm: v1alpha1.DefaultRealm,
			Credentials: map[string]string{
				"username": u.Username,
				"password": u.Password,
			},
		},
		Listeners: []v1alpha1.ListenerConfig{{
			Name:     "default-listener",
			Protocol: u.Protocol,
			Addr:     u.Address,
			Port:     u.Port,
			Routes:   []string{"allow-any"},
		}},
		Clusters: []v1alpha1.ClusterConfig{{
			Name:      "allow-any",
			Type:      "STATIC",
			Endpoints: []string{"0.0.0.0/0"},
		}},
	}

	if strings.ToUpper(u.Protocol) == "TLS" || strings.ToUpper(u.Protocol) == "DTLS" {
		certPem, keyPem, err := util.GenerateSelfSignedKey()
		if err != nil {
			return nil, err
		}
		c.Listeners[0].Cert = string(certPem)
		c.Listeners[0].Key = string(keyPem)
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}

	return c, nil
}

// LoadConfig loads a configuration from a file, substituting environment variables for
// placeholders in the configuration file. Returns the new configuration or error if load fails.
func LoadConfig(config string) (*v1alpha1.StunnerConfig, error) {
	c, err := os.ReadFile(config)
	if err != nil {
		return nil, fmt.Errorf("could not read config: %s\n", err.Error())
	}

	// substitute environtment variables
	// default port: STUNNER_PUBLIC_PORT -> STUNNER_PORT
	re := regexp.MustCompile(`^[0-9]+$`)
	port, ok := os.LookupEnv("STUNNER_PORT")
	if !ok || (ok && port == "") || (ok && !re.Match([]byte(port))) {
		publicPort := v1alpha1.DefaultPort
		publicPortStr, ok := os.LookupEnv("STUNNER_PUBLIC_PORT")
		if ok {
			if p, err := strconv.Atoi(publicPortStr); err == nil {
				publicPort = p
			}
		}
		os.Setenv("STUNNER_PORT", fmt.Sprintf("%d", publicPort))
	}

	e := os.ExpandEnv(string(c))

	s := v1alpha1.StunnerConfig{}
	// try YAML first
	if err = yaml.Unmarshal([]byte(e), &s); err != nil {
		// if it fails, try to json
		if errJ := json.Unmarshal([]byte(e), &s); err != nil {
			return nil, fmt.Errorf("could not parse config file at '%s': "+
				"YAML parse error: %s, JSON parse error: %s\n",
				config, err.Error(), errJ.Error())
		}
	}

	return &s, nil
}

// GetConfig returns the configuration of the running STUNner daemon.
func (s *Stunner) GetConfig() *v1alpha1.StunnerConfig {
	s.log.Tracef("GetConfig")

	// singletons, but we want to avoid panics when GetConfig is called on an uninitialized
	// STUNner object
	adminConf := v1alpha1.AdminConfig{}
	if len(s.adminManager.Keys()) > 0 {
		adminConf = *s.GetAdmin().GetConfig().(*v1alpha1.AdminConfig)
	}

	authConf := v1alpha1.AuthConfig{}
	if len(s.authManager.Keys()) > 0 {
		authConf = *s.GetAuth().GetConfig().(*v1alpha1.AuthConfig)
	}

	listeners := s.listenerManager.Keys()
	clusters := s.clusterManager.Keys()

	c := v1alpha1.StunnerConfig{
		ApiVersion: s.version,
		Admin:      adminConf,
		Auth:       authConf,
		Listeners:  make([]v1alpha1.ListenerConfig, len(listeners)),
		Clusters:   make([]v1alpha1.ClusterConfig, len(clusters)),
	}

	for i, name := range listeners {
		c.Listeners[i] = *s.GetListener(name).GetConfig().(*v1alpha1.ListenerConfig)
	}

	for i, name := range clusters {
		c.Clusters[i] = *s.GetCluster(name).GetConfig().(*v1alpha1.ClusterConfig)
	}

	return &c
}

type watchContext struct {
	config  string
	watcher *fsnotify.Watcher
	ch      chan<- v1alpha1.StunnerConfig
}

type watchContextKey string

// WatchConfig will watch a configuration file specified in the `config` parameter for changes and
// emit a new `StunnerConfig` on the channel specified in the `ch` argument each time the file
// changes. If no file exists at the given path it will periodically retry until the file
// appears. Note that the configuration sent through the channel is not validated, make sure to
// check for syntax errors on the receiver side. Use the `context` argument the cancel the watcher.
func (s *Stunner) WatchConfig(ctx context.Context, config string, ch chan<- v1alpha1.StunnerConfig) error {
	s.log.Tracef("WatchConfig")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// decorate context
	ctx = context.WithValue(ctx, watchContextKey("watchContext"), watchContext{
		config:  config,
		watcher: watcher,
		ch:      ch,
	})

	go func() {
		defer watcher.Close()

		for {
			// try to watch
			if ok := s.configWatcher(ctx); !ok {
				return
			}

			if ok := s.tryWatchConfig(ctx); !ok {
				return
			}
		}

	}()

	return nil
}

// tryWatchConfig runs a timer to look for the config file at the given path and returns it
// immediately once found. Returns true if further action is needed (configWatcher has to be
// started) or false on normal exit.
func (s *Stunner) tryWatchConfig(ctx context.Context) bool {
	s.log.Tracef("tryWatchConfig")

	watchContext := ctx.Value(watchContextKey("watchContext")).(watchContext)
	config := watchContext.config

	ticker := time.NewTicker(confUpdatePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false

		case <-ticker.C:
			s.log.Debugf("trying to read config file %q from periodic timer",
				config)

			// check if config file exists and it is readable
			if _, err := os.Stat(config); errors.Is(err, os.ErrNotExist) {
				s.log.Debugf("config file %q does not exist", config)

				// report status in every 10th second
				if time.Now().Second()%10 == 0 {
					s.log.Warnf("waiting for config file %q", config)
				}

				continue
			}

			return true
		}
	}
}

// configWatcher actually watches the config and emits the configs found on the specified
// channel. Returns true if further action is needed (tryWatachConfig is to be started) or false on
// normal exit.
func (s *Stunner) configWatcher(ctx context.Context) bool {
	s.log.Tracef("configWatcher")
	prev := v1alpha1.StunnerConfig{}

	watchContext := ctx.Value(watchContextKey("watchContext")).(watchContext)
	config := watchContext.config
	watcher := watchContext.watcher
	ch := watchContext.ch

	if err := watcher.Add(config); err != nil {
		s.log.Debugf("could not add config file %q watcher: %s", config, err.Error())
		return true
	}

	// emit an initial config
	c, err := LoadConfig(config)
	if err != nil {
		s.log.Warnf("could not load config file %q: %s", config, err.Error())
		return true
	}

	s.log.Debugf("config file successfully loaded from %q", config)

	// send a deepcopy over the channel
	copy := v1alpha1.StunnerConfig{}
	c.DeepCopyInto(&copy)
	ch <- copy

	// save deepcopy so that we can filter repeated events
	c.DeepCopyInto(&prev)

	for {
		select {
		case <-ctx.Done():
			return false

		case e, ok := <-watcher.Events:
			if !ok {
				s.log.Debug("config watcher event handler received invalid event")
				return true
			}

			s.log.Debugf("received watcher event: %s", e.String())

			if e.Has(fsnotify.Remove) {
				s.log.Warnf("config file deleted %q, disabling watcher", e.Op.String())

				if err := watcher.Remove(config); err != nil {
					s.log.Debugf("could not remove config file %q watcher: %s",
						config, err.Error())
				}

				return true
			}

			if !e.Has(fsnotify.Write) {
				s.log.Debugf("unhandled notify op on config file %q (ignoring): %s",
					e.Name, e.Op.String())
				continue
			}

			s.log.Debugf("loading configuration file: %s", config)
			c, err = LoadConfig(config)
			if err != nil {
				// assume it is a YAML/JSON syntax error (LoadConfig does not
				// validate): report and ignore
				s.log.Warnf("could not load config file %q: %s", config, err.Error())
				continue
			}

			// suppress repeated events
			if c.DeepEqual(&prev) {
				s.log.Debugf("ignoring recurrent notify event for the same config file")
				continue
			}

			s.log.Debugf("config file successfully loaded from %q", config)

			copy := v1alpha1.StunnerConfig{}
			c.DeepCopyInto(&copy)
			ch <- copy

			// save deepcopy so that we can filter repeated events
			c.DeepCopyInto(&prev)

		case err, ok := <-watcher.Errors:
			if !ok {
				s.log.Debugf("config watcher error handler received invalid error")
				return true
			}

			s.log.Debugf("watcher error, deactivating watcher: %s", err.Error())

			if err := watcher.Remove(config); err != nil {
				s.log.Debugf("could not remove config file %q watcher: %s",
					config, err.Error())
			}

			return true
		}
	}
}

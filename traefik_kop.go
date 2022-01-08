package traefikkop

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
	ptypes "github.com/traefik/paerser/types"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/provider/aggregator"
	"github.com/traefik/traefik/v2/pkg/provider/docker"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/server"
)

var Version = ""

const defaultEndpointPath = "/var/run/docker.sock"
const defaultThrottleDuration = 5 * time.Second

func Start(config Config) {

	_, err := os.Stat(defaultEndpointPath)
	if err != nil {
		logrus.Fatal(err)
	}

	dp := &docker.Provider{
		Endpoint:                "unix://" + defaultEndpointPath,
		HTTPClientTimeout:       ptypes.Duration(defaultTimeout),
		SwarmMode:               false,
		Watch:                   true,
		SwarmModeRefreshSeconds: ptypes.Duration(15 * time.Second),
	}

	store := NewStore(config.Hostname, config.Addr, config.Pass, config.DB)
	err = store.Ping()
	if err != nil {
		if strings.Contains(err.Error(), config.Addr) {
			logrus.Fatalf("failed to connect to redis: %s", err)
		}
		logrus.Fatalf("failed to connect to redis at %s: %s", config.Addr, err)
	}

	providers := &static.Providers{
		Docker: dp,
	}
	providerAggregator := aggregator.NewProviderAggregator(*providers)

	err = providerAggregator.Init()
	if err != nil {
		panic(err)
	}

	dockerClient, err := createDockerClient("unix://" + defaultEndpointPath)
	if err != nil {
		logrus.Fatalf("failed to create docker client: %s", err)
	}

	ctx := context.Background()
	routinesPool := safe.NewPool(ctx)

	watcher := server.NewConfigurationWatcher(
		routinesPool,
		providerAggregator,
		time.Duration(defaultThrottleDuration),
		[]string{},
		"docker",
	)

	watcher.AddListener(func(conf dynamic.Configuration) {
		// logrus.Printf("got new conf..\n")
		// fmt.Printf("%s\n", dumpJson(conf))
		logrus.Infoln("refreshing configuration")
		replaceIPs(dockerClient, &conf, config.BindIP)
		err := store.Store(conf)
		if err != nil {
			panic(err)
		}
	})

	watcher.Start()

	select {} // go forever
}

// replaceIPs for all service endpoints
//
// By default, traefik finds the local/internal docker IP for each container.
// Since we are exposing these services to an external node/server, we need
// to replace an IPs with the correct IP for this server, as configured at startup.
func replaceIPs(dockerClient client.APIClient, conf *dynamic.Configuration, ip string) {
	// modify HTTP URLs
	if conf.HTTP != nil && conf.HTTP.Services != nil {
		for svcName, svc := range conf.HTTP.Services {
			logrus.Debugf("found http service: %s", svcName)
			for i := range svc.LoadBalancer.Servers {
				server := &svc.LoadBalancer.Servers[i]
				if server.URL != "" {
					u, _ := url.Parse(server.URL)
					p := getContainerPort(dockerClient, "http", svcName, u.Port())
					if p != "" {
						u.Host = ip + ":" + p
					} else {
						u.Host = ip
					}
					server.URL = u.String()
				} else {
					scheme := "http"
					if server.Scheme != "" {
						scheme = server.Scheme
					}
					server.URL = fmt.Sprintf("%s://%s", scheme, ip)
					port := getContainerPort(dockerClient, "http", svcName, server.Port)
					if port != "" {
						server.URL += ":" + server.Port
					}
				}
			}
		}
	}

	// TCP
	if conf.TCP != nil && conf.TCP.Services != nil {
		for svcName, svc := range conf.TCP.Services {
			logrus.Debugf("found tcp service: %s", svcName)
			for i := range svc.LoadBalancer.Servers {
				server := &svc.LoadBalancer.Servers[i]
				server.Address = ip
				server.Port = getContainerPort(dockerClient, "tcp", svcName, server.Port)
			}
		}
	}

	// UDP
	if conf.UDP != nil && conf.UDP.Services != nil {
		for svcName, svc := range conf.UDP.Services {
			logrus.Debugf("found udp service: %s", svcName)
			for i := range svc.LoadBalancer.Servers {
				server := &svc.LoadBalancer.Servers[i]
				server.Address = ip
				server.Port = getContainerPort(dockerClient, "udp", svcName, server.Port)
			}
		}
	}
}

// Get host-port binding from container, if not explicitly set via labels
func getContainerPort(dockerClient client.APIClient, svcType string, svcName string, port string) string {
	svcName = strings.TrimSuffix(svcName, "@docker")
	container, err := findContainerByServiceName(dockerClient, svcType, svcName)
	if err != nil {
		logrus.Warn(err)
		return port
	}
	if isPortSet(container, svcType, svcName) {
		logrus.Debugf("using explicitly set port %s for %s", port, svcName)
		return port
	}
	exposedPort, err := getPortBinding(container)
	if err != nil {
		logrus.Warn(err)
		return port
	}
	if exposedPort == "" {
		logrus.Warnf("failed to find host-port for service %s", svcName)
		return port
	}
	logrus.Debugf("overriding service port from container host-port: using %s (was %s) for %s", exposedPort, port, svcName)
	return exposedPort
}

package vulcand2

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mailgun/vulcand/api"
	"github.com/mailgun/vulcand/engine"
	"github.com/mailgun/vulcand/plugin"

	"github.com/gliderlabs/registrator/bridge"
)

type HealthCheck struct {
	serviceID string
	url       string
	interval  time.Duration
	channel   chan bool
	status    bool
}

var healthChecks map[string]*HealthCheck

func init() {
	bridge.Register(new(Factory), "vulcand2")
	healthChecks = make(map[string]*HealthCheck)
}

type Factory struct{}

func (f *Factory) New(uri *url.URL) bridge.RegistryAdapter {
	var url string
	if uri.Host != "" {
		url = "http://" + uri.Host
	}
	var registry *plugin.Registry
	return &Vulcand2Adapter{client: api.NewClient(url, registry)}
}

type Vulcand2Adapter struct {
	client *api.Client
}

func (r *Vulcand2Adapter) Ping() error {
	if err := r.client.GetStatus(); err != nil {
		return err
	}
	log.Println("Ping vulcand")
	return nil
}

func (r *Vulcand2Adapter) buildURL(service *bridge.Service) string {
	protocol := "http"
	if service.Port == 443 || service.Port == 8443 {
		protocol = "https"
	}
	url := protocol + "://" + net.JoinHostPort(service.IP, strconv.Itoa(service.Port))
	return url
}

func (r *Vulcand2Adapter) healthChecker(healthCheck *HealthCheck) {
	for {
		select {
		case <-healthCheck.channel:
			return
		default:
			timeout := time.Duration(5 * time.Second)
			client := http.Client{Timeout: timeout}
			response, err := client.Get(healthCheck.url)
			healthCheck.status = false
			if err == nil {
				defer response.Body.Close()
				if response.StatusCode == http.StatusOK {
					healthCheck.status = true
				}
				log.Printf("Health check %s (%s) result HTTP code %d", healthCheck.serviceID, healthCheck.url, response.StatusCode)
			} else {
				log.Printf("Error performing health check %s (%s): %v", healthCheck.serviceID, healthCheck.url, err)
			}
			time.Sleep(healthCheck.interval)
		}
	}
}

func (r *Vulcand2Adapter) registerFrontend(frontendId string, frontendHost string, backendId string, ttl time.Duration) error {
	f, err := engine.NewHTTPFrontend(frontendId, backendId, `Host("`+frontendHost+`") && PathRegexp(".*")`, engine.HTTPFrontendSettings{})
	if err != nil {
		return err
	}
	if err := r.client.UpsertFrontend(*f, ttl); err != nil {
		return err
	}
	return nil
}

func (r *Vulcand2Adapter) registerBackend(backendId string) error {
	b, err := engine.NewHTTPBackend(backendId, engine.HTTPBackendSettings{})
	if err != nil {
		return err
	}
	if err := r.client.UpsertBackend(*b); err != nil {
		return err
	}
	return nil
}

func (r *Vulcand2Adapter) registerServer(serverId string, serverURL string, backendId string, ttl time.Duration) error {
	srv, err := engine.NewServer(serverId, serverURL)
	if err != nil {
		return err
	}
	bk := &engine.BackendKey{Id: backendId}
	if err := r.client.UpsertServer(*bk, *srv, ttl); err != nil {
		return err
	}
	return nil
}

func (r *Vulcand2Adapter) Register(service *bridge.Service) error {
	serverURL := r.buildURL(service)
	log.Printf("Registering service ID %s, name %s, port %d, IP %s, URL %s", service.ID, service.Name, service.Port, service.IP, serverURL)
	return r.Refresh(service)
}

func (r *Vulcand2Adapter) Deregister(service *bridge.Service) error {
	if healthCheck, exists := healthChecks[service.ID]; exists {
		healthCheck.channel <- false
		delete(healthChecks, service.ID)
	}
	sk := engine.ServerKey{BackendKey: engine.BackendKey{Id: service.Name}, Id: service.ID}
	return r.client.DeleteServer(sk)
	// :TODO: vulcand doet geen TTL bij backends, moeten we hier nog iets mee?
}

func (r *Vulcand2Adapter) Refresh(service *bridge.Service) error {
	log.Printf("Refreshing service ID %s", service.ID)

	serverURL := r.buildURL(service)
	ttl := time.Duration(service.TTL) * time.Second

	if frontendHostCTR := service.Attrs["frontend_host_container"]; frontendHostCTR != "" {
		// Individuele containers altijd registreren indien gewenst, zonder naar health checks te kijken
		if err := r.registerBackend(service.ID); err != nil {
			return err
		}
		if err := r.registerFrontend(service.ID, frontendHostCTR, service.ID, ttl); err != nil {
			return err
		}
		if err := r.registerServer(service.ID, serverURL, service.ID, ttl); err != nil {
			return err
		}
	}

	if frontendHostLB := service.Attrs["frontend_host_load_balancer"]; frontendHostLB != "" {
		if healthCheck, exists := healthChecks[service.ID]; exists {
			log.Println("Health status for", service.ID, healthCheck.status)
			if !healthCheck.status {
				return nil
			}
		}

		if checkHTTP := service.Attrs["check_http"]; checkHTTP != "" {
			if _, exists := healthChecks[service.ID]; !exists {
				checkInterval := 11 * time.Second
				if i := service.Attrs["check_interval"]; i != "" {
					val, err := time.ParseDuration(i)
					if err != nil {
						return err
					}
					checkInterval = val
				}
				log.Printf("Registering health check for %s, interval %s", service.ID, checkInterval)
				healthCheck := HealthCheck{service.ID, serverURL + checkHTTP, checkInterval, make(chan bool), false}
				healthChecks[service.ID] = &healthCheck
				go r.healthChecker(&healthCheck)
			}
		}

		if err := r.registerBackend(service.Name); err != nil {
			return err
		}
		if err := r.registerFrontend(service.Name, frontendHostLB, service.Name, ttl); err != nil {
			return err
		}
		return r.registerServer(service.ID, serverURL, service.Name, ttl)
	}

	return nil
}

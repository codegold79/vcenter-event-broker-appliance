package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"

	handler "github.com/openfaas/templates-sdk/go-http"
	"github.com/pelletier/go-toml"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const secretPath = "/var/openfaas/secrets/vcconfig"

// vcConfig represents the toml vcconfig file
type vcConfig struct {
	VCenter struct {
		Server   string
		User     string
		Password string
		Insecure bool
	}
}

// vsClient stores vSphere connection information.
type vsClient struct {
	govmomi *govmomi.Client
	rest    *rest.Client
	tagMgr  *tags.Manager
}

// cloudEvent stores incoming event data.
type cloudEvent struct {
	Data types.AlarmStatusChangedEvent
}

// Handle a function invocation
func Handle(req handler.Request) (handler.Response, error) {
	ctx := context.Background()

	cloudEvt, err := parseCloudEvent(req.Body)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("parsing cloud event data: %w", err))
	}

	// Determine if data AlarmStatusChangedEvent is correct.
	if !isStorageInAlarm(cloudEvt) {
		message := "Alert not for CPU/Memory in red, nothing to do."
		log.Println(message)

		return handler.Response{
			Body:       []byte(message),
			StatusCode: http.StatusOK,
		}, nil
	}

	// Load config every time, to ensure the most updated version is used.
	cfg, err := loadTomlCfg(secretPath)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("loading of vcconfig: %w", err))
	}

	vsClient, err := newClient(ctx, cfg)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("connecting to vSphere: %w", err))
	}

	vmMOR := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: "vm-1047",
	}

	moVM, err := vsClient.moVirtualMachine(ctx, vmMOR)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("getting vm configs: %w", err))
	}

	message := fmt.Sprintf("moVM: %+v\n", moVM)
	log.Println(message)

	return handler.Response{
		Body:       []byte(message),
		StatusCode: http.StatusOK,
	}, nil
}

func errRespondAndLog(err error) (handler.Response, error) {
	if debug() {
		log.Println(err.Error())
	}

	return handler.Response{
		Body:       []byte(err.Error()),
		StatusCode: http.StatusInternalServerError,
	}, err
}

// Debug determines verbose logging
func debug() bool {
	verbose := os.Getenv("write_debug")

	if verbose == "true" {
		return true
	}

	return false
}

func parseCloudEvent(req []byte) (cloudEvent, error) {
	var event cloudEvent

	err := json.Unmarshal(req, &event)
	if err != nil {
		return cloudEvent{}, fmt.Errorf("unmarshalling json: %w", err)
	}

	if err := isValidEvent(event); err != nil {
		return cloudEvent{}, err
	}

	return event, nil
}

func isValidEvent(event cloudEvent) error {
	if event.Data.Vm == nil || event.Data.Vm.Vm.Value == "" {
		return errors.New("empty VM managed object reference")
	}

	if event.Data.Alarm.Name == "" || event.Data.To == "" {
		return errors.New("insufficent alarm infomration")
	}

	return nil
}

func isStorageInAlarm(event cloudEvent) bool {
	alarm := false

	if event.Data.To == "red" && (event.Data.Alarm.Name == "VM Memory Usage" || event.Data.Alarm.Name == "VM CPU Usage") {
		alarm = true
	}

	return alarm
}

func loadTomlCfg(path string) (*vcConfig, error) {
	var cfg vcConfig

	secret, err := toml.LoadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading vcconfig.toml: %w", err)
	}

	err = secret.Unmarshal(&cfg)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling vcconfig.toml: %w", err)
	}

	err = validateConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}

// ValidateConfig ensures the bare minimum of information is in the config file.
func validateConfig(cfg vcConfig) error {
	reqFields := map[string]string{
		"vcenter server":   cfg.VCenter.Server,
		"vcenter user":     cfg.VCenter.User,
		"vcenter password": cfg.VCenter.Password,
	}

	// Multiple fields may be missing, but err on the first encountered.
	for k, v := range reqFields {
		if v == "" {
			return errors.New("required field(s) missing, including " + k)
		}
	}

	return nil
}

// newClient connects to vSphere govmomi API
func newClient(ctx context.Context, cfg *vcConfig) (*vsClient, error) {
	u := url.URL{
		Scheme: "https",
		Host:   cfg.VCenter.Server,
		Path:   "sdk",
	}

	u.User = url.UserPassword(cfg.VCenter.User, cfg.VCenter.Password)
	insecure := cfg.VCenter.Insecure

	gc, err := govmomi.NewClient(ctx, &u, insecure)
	if err != nil {
		return nil, fmt.Errorf("connecting to vSphere API: %w", err)
	}

	rc := rest.NewClient(gc.Client)
	tm := tags.NewManager(rc)

	vsc := vsClient{
		govmomi: gc,
		rest:    rc,
		tagMgr:  tm,
	}

	err = vsc.rest.Login(ctx, u.User)
	if err != nil {
		return nil, fmt.Errorf("logging into rest api: %w", err)
	}

	return &vsc, nil
}

// unappliedConfigs returns configurations that are not current.
func (c *vsClient) moVirtualMachine(ctx context.Context, mor types.ManagedObjectReference) (mo.VirtualMachine, error) {
	// Look for current hardware configuration
	var moVM mo.VirtualMachine

	pc := property.DefaultCollector(c.govmomi.Client)
	pc.Retrieve(ctx, []types.ManagedObjectReference{mor}, []string{}, &moVM)

	log.Printf("\nvm moRef (vmMOR): %v\n", mor)
	log.Printf("\nmoVM: %+v\n", moVM)
	log.Printf("\nclient: %+v\n", c)

	if moVM.Config == nil {
		log.Printf("\nno config info in vm: %+v\n", moVM)
		return mo.VirtualMachine{}, errors.New("no config info in vm")
	}

	return moVM, nil
}

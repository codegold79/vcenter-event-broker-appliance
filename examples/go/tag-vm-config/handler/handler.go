package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	handler "github.com/openfaas-incubator/go-function-sdk"
	"github.com/pelletier/go-toml"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const cfgPath = "/var/openfaas/secrets/vcconfig"

// vcConfig represents the toml vcconfig file
type vcConfig struct {
	VCenter struct {
		Server   string
		User     string
		Password string
		Insecure bool
	}
}

// cloudEvent contains event data.
type cloudEvent struct {
	Data types.AlarmStatusChangedEvent
}

var (
	lock   sync.Mutex // Lock protects client.
	client *vsClient  // Client persists vSphere connection.
	once   sync.Once  // For handleSignal() to be called once.
)

// Handle a function invocation
func Handle(req handler.Request) (handler.Response, error) {
	ctx := context.Background()

	cloudEvt, err := parseCloudEvent(req.Body)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("parsing cloud event data: %w", err)), err
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
	cfg, err := loadTomlCfg(cfgPath)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("loading of vcconfig: %w", err)), err
	}

	// Connect to vSphere govmomi API once and persist connection with global variable.
	err = vsConnect(ctx, cfg)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("connecting to vSphere: %w", err)), err
	}

	once.Do(func() {
		// Set up os signal handling to log out of vSphere.
		go handleSignal(ctx)
	})

	// Retrieve the Managed Object Reference from the event.
	vmMOR, err := eventVmMoRef(cloudEvt)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("retrieving VM managed reference object: %w", err)), err
	}

	moVM := moVirtualMachine(ctx, *vmMOR)

	catID, tagID, err := selectTag(ctx, cloudEvt, moVM)
	if err != nil {
		return errRespondAndLog(fmt.Errorf("retrieving VM managed reference object: %w", err)), err
	}

	//vm := object.NewVirtualMachine(client.govmomi.Client, *vmMOR)
	// TODO: list attached tags and remove any in the cateogry
	// of the alarm, except for if the tagID matches. If tag id matches,
	// there is no need to attach it in next step.

	err = client.tagMgr.AttachTag(ctx, tagID, vmMOR)
	if err != nil {
		wrapErr := fmt.Errorf("tagging managed reference object: %w", err)

		if debug() {
			log.Println(wrapErr)
		}

		return errRespondAndLog(wrapErr), err
	}

	message := fmt.Sprintf("%v was tagged with %v, %v", vmMOR.Value, tagID, catID)
	log.Println(message)

	return handler.Response{
		Body:       []byte(message),
		StatusCode: http.StatusOK,
	}, nil
}

func errRespondAndLog(err error) handler.Response {

	if debug() {
		log.Println(err.Error())
	}

	return handler.Response{
		Body:       []byte(err.Error()),
		StatusCode: http.StatusInternalServerError,
	}
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
			return errors.New("required field(s) missing in config, including " + k)
		}
	}

	return nil
}

// vsConnect connects to vSphere govmomi API using information from vcconfig.toml.
func vsConnect(ctx context.Context, cfg *vcConfig) error {
	lock.Lock()
	defer lock.Unlock()

	if client == nil {
		u := url.URL{
			Scheme: "https",
			Host:   cfg.VCenter.Server,
			Path:   "sdk",
		}
		u.User = url.UserPassword(cfg.VCenter.User, cfg.VCenter.Password)
		insecure := cfg.VCenter.Insecure

		if debug() {
			log.Println("connecting to vSphere")
		}

		c, err := newClient(ctx, u, insecure)
		if err != nil {
			return fmt.Errorf("connecting to vSphere API: %w", err)
		}

		// Set global variable to persist connection.
		client = c
	}

	return nil
}

// Debug determines verbose logging
func debug() bool {
	verbose := os.Getenv("write_debug")

	if verbose == "true" {
		return true
	}

	return false
}

func handleSignal(ctx context.Context) {
	var sigCh = make(chan os.Signal, 2)

	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)

	s := <-sigCh
	verbose := debug()

	if verbose {
		log.Printf("got signal: %v, log out of vSphere", s)
	}

	err := client.logout(ctx)
	if verbose {
		if err != nil {
			log.Printf("vSphere logout failed: %v", err)
			return
		}
		log.Println("logged out of govmomi and rest APIs")
	}
}

func eventVmMoRef(event cloudEvent) (*types.ManagedObjectReference, error) {
	// Fill information in the request into a govmomi type.
	moRef := types.ManagedObjectReference{
		Type:  event.Data.Vm.Vm.Type,
		Value: event.Data.Vm.Vm.Value,
	}

	return &moRef, nil
}

// moVirtualMachine contains current VM config information.
func moVirtualMachine(ctx context.Context, vmMOR types.ManagedObjectReference) mo.VirtualMachine {
	var moVM mo.VirtualMachine
	log.Printf("\nvmMOR is %+v\n", vmMOR)

	pc := property.DefaultCollector(client.govmomi.Client)
	err := pc.Retrieve(ctx, []types.ManagedObjectReference{vmMOR}, []string{}, &moVM)
	if err != nil {
		log.Printf("\nERROR! %v\n", err)
	}
	log.Printf("\nmoVM is %+v\n", moVM)
	return moVM
}

// selectTag finds the current config value for the type, and will select
// the tag that is an increment above it (but below the limits).
func selectTag(ctx context.Context, ce cloudEvent, moVM mo.VirtualMachine) (string, string, error) {
	catName := catName(ce.Data.Alarm.Name)
	tagName := ""
	// get the expected name of the tag (incremented value)

	switch catName {
	case "config.hardware.numCPU":
		// CPU tags are easy. Just increment it up to the max of 4.
		tagName = incCpuVal(int(moVM.Config.Hardware.NumCPU))
	case "config.hardware.memoryMB":
		// Mem tags are a bit tricky. Gotta find the exponent to the 2 base.
		// Then, increment the exponent up to the max of 23 (8 gb RAM).
		tagName = incMemVal(float64(moVM.Config.Hardware.MemoryMB))
	}

	tagList, err := client.tagMgr.GetTagsForCategory(ctx, catName)
	if err != nil {
		return "", "", err
	}

	catID, tagID := findCatAndTagID(tagList, tagName)

	// return the tag ID given the name.
	return catID, tagID, nil
}

// catName returns the category name based on alarm name.
func catName(alarmName string) string {
	switch alarmName {
	case "VM CPU Usage":
		return "config.hardware.numCPU"
	case "VM Memory Usage":
		return "config.hardware.memoryMB"
	}

	return ""
}

func incCpuVal(numCPU int) string {
	newNum := 4
	numCPU++
	if numCPU < newNum {
		newNum = numCPU
	}
	return strconv.Itoa(newNum)
}

func incMemVal(mem float64) string {
	maxExp := 23
	newMem := 1 << maxExp

	exp := int(math.Log10(mem) / math.Log10(2))
	exp++

	if exp < maxExp {
		newMem = 1 << exp
	}

	return strconv.Itoa(newMem)
}

func findCatAndTagID(ts []tags.Tag, tn string) (string, string) {
	for _, t := range ts {
		if t.Name == tn {
			return t.CategoryID, t.ID
		}
	}

	return "", ""
}

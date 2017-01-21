package baasil

import (
	"fmt"
	"io/ioutil"
	"strings"
	"time"
	"os"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/packethost/packngo"
)

const (
	dockerConfigDir = "/etc/docker"
	consumerToken   = "24e70949af5ecd17fe8e867b335fc88e7de8bd4ad617c0403d8769a376ddea72"
)

var _ drivers.Driver = &Driver{}

type Driver struct {
	*drivers.BaseDriver
	ApiKey          string
	ProjectID       string
	Plan            string
	Facility        string
	OperatingSystem string
	BillingCycle    string
	DeviceID        string
	UserData        string
	Tags            []string
	CaCertPath      string
	SSHKeyID        string
	UserDataFile    string
}

// NewDriver is a backward compatible Driver factory method.  Using
// new(baasil.Driver) is preferred.
func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			Name:   "baasil-api-key",
			Usage:  "Baasil api key",
			EnvVar: "BAASIL",
		},
		mcnflag.StringFlag{
			Name:   "baasil-project-id",
			Usage:  "Baasil Project Id",
			EnvVar: "BAASIL_PROJECT_ID",
		},
		mcnflag.StringFlag{
			Name:   "baasil-os",
			Usage:  fmt.Sprintf("Baasil OS, possible values are: %v", strings.Join(d.getOsFlavors(), ", ")),
			Value:  "ubuntu_14_04",
			EnvVar: "BAASIL_OS",
		},
		mcnflag.StringFlag{
			Name:   "baasil-facility-code",
			Usage:  "Baasil facility code",
			Value:  "ewr1",
			EnvVar: "BAASIL_FACILITY_CODE",
		},
		mcnflag.StringFlag{
			Name:   "baasil-plan",
			Usage:  "Baasil Server Plan",
			Value:  "baremetal_0",
			EnvVar: "BAASIL_PLAN",
		},
		mcnflag.StringFlag{
			Name:   "baasil-billing-cycle",
			Usage:  "Baasil billing cycle, hourly or monthly",
			Value:  "hourly",
			EnvVar: "BAASIL_BILLING_CYCLE",
		},
		mcnflag.StringFlag{
			Name:   "baasil-userdata",
			Usage:  "Path to file with cloud-init user-data",
			EnvVar: "BAASIL_USERDATA",
		},
	}
}

func (d *Driver) DriverName() string {
	return "baasil"
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	if strings.Contains(flags.String("baasil-os"), "coreos") {
		d.SSHUser = "core"
	}
	if strings.Contains(flags.String("baasil-os"), "rancher") {
		d.SSHUser = "rancher"
	}

	d.ApiKey = flags.String("baasil-api-key")
	d.ProjectID = flags.String("baasil-project-id")
	d.OperatingSystem = flags.String("baasil-os")
	d.Facility = flags.String("baasil-facility-code")
	d.Plan = flags.String("baasil-plan")
	d.BillingCycle = flags.String("baasil-billing-cycle")
	d.UserDataFile = flags.String("baasil-userdata")

	if d.ApiKey == "" {
		return fmt.Errorf("baasil driver requires the --baasil-api-key option")
	}
	if d.ProjectID == "" {
		return fmt.Errorf("baasil driver requires the --baasil-project-id option")
	}

	return nil
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) PreCreateCheck() error {
	if d.UserDataFile != "" {
		if _, err := os.Stat(d.UserDataFile); os.IsNotExist(err) {
			return fmt.Errorf("user-data file %s could not be found", d.UserDataFile)
		}
	}

	flavors := d.getOsFlavors()
	if !stringInSlice(d.OperatingSystem, flavors) {
		return fmt.Errorf("specified --baasil-os not one of %v", strings.Join(flavors, ", "))
	}

	client := d.getClient()
	facilities, _, err := client.Facilities.List()
	if err != nil {
		return err
	}
	for _, facility := range facilities {
		if facility.Code == d.Facility {
			return nil
		}
	}

	return fmt.Errorf("baasil requires a valid facility")
}

func (d *Driver) Create() error {
	var userdata string
	if d.UserDataFile != "" {
		buf, err := ioutil.ReadFile(d.UserDataFile)
		if err != nil {
			return err
		}
		userdata = string(buf)
	}

	log.Info("Creating SSH key...")

	key, err := d.createSSHKey()
	if err != nil {
		return err
	}

	d.SSHKeyID = key.ID

	client := d.getClient()
	createRequest := &packngo.DeviceCreateRequest{
		HostName:     d.MachineName,
		Plan:         d.Plan,
		Facility:     d.Facility,
		OS:           d.OperatingSystem,
		BillingCycle: d.BillingCycle,
		ProjectID:    d.ProjectID,
		UserData:     userdata,
		Tags:         d.Tags,
	}

	log.Info("Provisioning Baasil server...")
	newDevice, _, err := client.Devices.Create(createRequest)
	if err != nil {
		return err
	}
	t0 := time.Now()

	d.DeviceID = newDevice.ID

	for {
		newDevice, _, err = client.Devices.Get(d.DeviceID)
		if err != nil {
			return err
		}

		for _, ip := range newDevice.Network {
			if ip.Public && ip.AddressFamily == 4 {
				d.IPAddress = ip.Address
			}
		}

		if d.IPAddress != "" {
			break
		}

		time.Sleep(1 * time.Second)
	}

	log.Infof("Created device ID %s, IP address %s",
		newDevice.ID,
		d.IPAddress)

	log.Info("Waiting for Provisioning...")
	stage := float32(0)
	for {
		newDevice, _, err = client.Devices.Get(d.DeviceID)
		if err != nil {
			return err
		}
		if newDevice.State == "provisioning" && stage != newDevice.ProvisionPer {
			stage = newDevice.ProvisionPer
			log.Debugf("Provisioning %v%% complete", newDevice.ProvisionPer)
		}
		if newDevice.State == "active" {
			log.Debugf("Device State: %s", newDevice.State)
			break
		}
		time.Sleep(10 * time.Second)
	}

	log.Debugf("Provision time: %v.", time.Since(t0))

	log.Debug("Waiting for SSH...")
	if err := drivers.WaitForSSH(d); err != nil {
		return err
	}

	return nil
}

func (d *Driver) createSSHKey() (*packngo.SSHKey, error) {
	sshKeyPath := d.GetSSHKeyPath()
	log.Debugf("Writing SSH Key to %s", sshKeyPath)

	if err := ssh.GenerateSSHKey(sshKeyPath); err != nil {
		return nil, err
	}

	publicKey, err := ioutil.ReadFile(sshKeyPath + ".pub")
	if err != nil {
		return nil, err
	}

	createRequest := &packngo.SSHKeyCreateRequest{
		Label: fmt.Sprintf("docker machine: %s", d.MachineName),
		Key:   string(publicKey),
	}

	key, _, err := d.getClient().SSHKeys.Create(createRequest)
	if err != nil {
		return key, err
	}

	return key, nil
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetIP() (string, error) {
	if d.IPAddress == "" {
		return "", fmt.Errorf("IP address is not set")
	}
	return d.IPAddress, nil
}

func (d *Driver) GetState() (state.State, error) {
	device, _, err := d.getClient().Devices.Get(d.DeviceID)
	if err != nil {
		return state.Error, err
	}

	switch device.State {
	case "queued", "provisioning", "powering_on":
		return state.Starting, nil
	case "active":
		return state.Running, nil
	case "powering_off":
		return state.Stopping, nil
	case "inactive":
		return state.Stopped, nil
	}
	return state.None, nil
}

func (d *Driver) Start() error {
	_, err := d.getClient().Devices.PowerOn(d.DeviceID)
	return err
}

func (d *Driver) Stop() error {
	_, err := d.getClient().Devices.PowerOff(d.DeviceID)
	return err
}

func (d *Driver) Remove() error {
	client := d.getClient()

	if _, err := client.SSHKeys.Delete(d.SSHKeyID); err != nil {
		if er, ok := err.(*packngo.ErrorResponse); !ok || er.Response.StatusCode != 404 {
			return err
		}
	}

	if _, err := client.Devices.Delete(d.DeviceID); err != nil {
		if er, ok := err.(*packngo.ErrorResponse); !ok || er.Response.StatusCode != 404 {
			return err
		}
	}
	return nil
}

func (d *Driver) Restart() error {
	_, err := d.getClient().Devices.Reboot(d.DeviceID)
	return err
}

func (d *Driver) Kill() error {
	_, err := d.getClient().Devices.PowerOff(d.DeviceID)
	return err
}

func (d *Driver) GetDockerConfigDir() string {
	return dockerConfigDir
}

func (d *Driver) getClient() *packngo.Client {
	return packngo.NewClient(consumerToken, d.ApiKey, nil)
}

func (d *Driver) getOsFlavors() []string {
	return []string{"centos_7", "coreos_alpha", "coreos_beta", "coreos_stable", "debian_8", "freebsd_10_8", "rancher", "ubuntu_14_04", "ubuntu_16_04"}
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

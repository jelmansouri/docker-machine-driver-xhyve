package xhyve

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/zchee/docker-machine-xhyve/xhyve"
)

const (
	isoFilename = "boot2docker.iso"
)

type Driver struct {
	*drivers.BaseDriver
	Memory         int
	DiskSize       int
	CPU            int
	TmpISO         string
	UUID           string
	BootCmd        string
	Boot2DockerURL string
	CaCertPath     string
	PrivateKeyPath string
}

var (
	ErrMachineExist    = errors.New("machine already exists")
	ErrMachineNotExist = errors.New("machine does not exist")
)

// RegisterCreateFlags registers the flags this driver adds to
// "docker hosts create"
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.Flag{
			EnvVar: "XHYVE_BOOT2DOCKER_URL",
			Name:   "xhyve-boot2docker-url",
			Usage:  "The URL of the boot2docker image. Defaults to the latest available version",
			Value:  "",
		},
		mcnflag.Flag{
			EnvVar: "XHYVE_CPU_COUNT",
			Name:   "xhyve-cpu-count",
			Usage:  "Number of CPUs for the machine (-1 to use the number of CPUs available)",
			Value:  1,
		},
		mcnflag.Flag{
			EnvVar: "XHYVE_MEMORY_SIZE",
			Name:   "xhyve-memory",
			Usage:  "Size of memory for host in MB",
			Value:  1024,
		},
		mcnflag.Flag{
			EnvVar: "XHYVE_DISK_SIZE",
			Name:   "xhyve-disk-size",
			Usage:  "Size of disk for host in MB",
			Value:  20000,
		},
		mcnflag.Flag{
			EnvVar: "XHYVE_BOOT_CMD",
			Name:   "xhyve-boot-cmd",
			Usage:  "Command of booting kexec protocol",
			Value:  "loglevel=3 user=docker console=ttyS0 console=tty0 noembed nomodeset norestore waitusb=10:LABEL=boot2docker-data base host=boot2docker",
		},
	}
}

func (d *Driver) GetMachineName() string {
	return d.MachineName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHKeyPath() string {
	return filepath.Join(d.LocalArtifactPath("."), "id_rsa")
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = 22
	}

	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "docker"
	}

	return d.SSHUser
}

func (d *Driver) DriverName() string {
	return "xhyve"
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.Boot2DockerURL = flags.String("xhyve-boot2docker-url")
	d.CPU = flags.Int("xhyve-cpu-count")
	d.Memory = flags.Int("xhyve-memory")
	d.DiskSize = flags.Int("xhyve-disk-size")
	d.BootCmd = flags.String("xhyve-boot-cmd")
	d.SwarmMaster = flags.Bool("swarm-master")
	d.SwarmHost = flags.String("swarm-host")
	d.SwarmDiscovery = flags.String("swarm-discovery")
	d.SSHUser = "docker"
	d.SSHPort = 22

	return nil
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", nil
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetIP() (string, error) {
	s, err := d.GetState()
	if err != nil {
		return "", err
	}
	if s != state.Running {
		return "", drivers.ErrHostIsNotRunning
	}

	ip, err := d.getIPfromDHCPLease()
	if err != nil {
		return "", err
	}

	return ip, nil
}

func (d *Driver) GetState() (state.State, error) { // TODO
	// VMRUN only tells use if the vm is running or not
	//	if stdout, _, _ := vmrun("list"); strings.Contains(stdout, d.vmxPath()) {
	return state.Running, nil
	//	}
	//	return state.Stopped, nil
}

// Check VirtualBox version
func (d *Driver) PreCreateCheck() error {
	ver, err := vboxVersionDetect()
	if err != nil {
		return fmt.Errorf("Error detecting VBox version: %s", err)
	}
	if !strings.HasPrefix(ver, "5") {
		return fmt.Errorf("Virtual Box version 4 or lower will cause a kernel panic if xhyve tries to run." +
			"You are running version: " +
			ver +
			"\n\t Please upgrade to version 5 at https://www.virtualbox.org/wiki/Downloads")
	}
	return nil
}

func (d *Driver) Create() error {

	b2dutils := mcnutils.NewB2dUtils("", "", d.GlobalArtifactPath())
	if err := b2dutils.CopyIsoToMachineDir(d.Boot2DockerURL, d.MachineName); err != nil {
		return err
	}

	log.Infof("Creating SSH key...")
	if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
		return err
	}

	log.Infof("Creating VM...")
	if err := os.MkdirAll(d.LocalArtifactPath("."), 0755); err != nil {
		return err
	}

	log.Debugf("Extracting vmlinuz64 and initrd.img from %s...", isoFilename)
	if err := d.extractKernelImages(); err != nil {
		return err
	}

	log.Debugf("Make a boot2docker userdata.tar key bundle...")
	if err := d.generateKeyBundle(); err != nil {
		return err
	}

	log.Infof("Creating Blank disk image...")
	if err := d.generateBlankDiskImage(d.DiskSize); err != nil {
		return err
	}
	log.Debugf("disk image: %d", d.DiskSize)

	log.Infof("Generate UUID...")
	d.UUID = uuidgen()
	log.Debugf("uuid: %s", d.UUID)

	log.Infof("Starting %s...", d.MachineName)
	if err := d.Start(); err != nil {
		return err
	}

	var ip string
	var err error

	log.Infof("Waiting for VM to come online...")
	for i := 1; i <= 60; i++ {
		ip, err = d.getIPfromDHCPLease()
		if err != nil {
			log.Debugf("Not there yet %d/%d, error: %s", i, 60, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if ip != "" {
			log.Debugf("Got an ip: %s", ip)
			break
		}
	}

	if ip == "" {
		return fmt.Errorf("Machine didn't return an IP after 120 seconds, aborting")
	}

	// we got an IP, let's copy ssh keys over
	d.IPAddress = ip

	return nil
}

func (d *Driver) Start() error {
	log.Infof("Creating %s xhyve VM...", d.MachineName)
	vmlinuz := fmt.Sprint("/Users/zchee/.docker/machine/machines/xhyve-test/vmlinuz64")
	initrd := fmt.Sprint("/Users/zchee/.docker/machine/machines/xhyve-test/initrd.img")
	uuid := d.UUID
	bootcmd := d.BootCmd

	args := strings.Fields("-A -s 0:0,hostbridge -s 31,lpc -l com1 -s 2:0,virtio-net")
	go xhyve.Exec(append(
		args,
		fmt.Sprintf("-m %dM", d.Memory),
		fmt.Sprintf("-s 3,ahci-cd,%s", path.Join(d.LocalArtifactPath("."), isoFilename)),
		fmt.Sprintf("-s 4,virtio-blk,%s", path.Join(d.LocalArtifactPath("."), d.MachineName+".img")),
		fmt.Sprintf("-U %s", uuid),
		"-f", fmt.Sprintf("kexec,%s,%s,%s", vmlinuz, initrd, bootcmd))...)

	log.Debugf("args: %s", args)

	return nil
}

func (d *Driver) Stop() error { // TODO
	// xhyve("controlvm", d.MachineName, "acpipowerbutton")
	for {
		s, err := d.GetState()
		if err != nil {
			return err
		}
		if s == state.Running {
			time.Sleep(1 * time.Second)
		} else {
			break
		}
	}

	d.IPAddress = ""

	return nil
}

func (d *Driver) Remove() error { // TODO
	s, err := d.GetState()
	if err != nil {
		if err == ErrMachineNotExist {
			log.Infof("machine does not exist, assuming it has been removed already")
			return nil
		}
		return err
	}
	if s == state.Running {
		if err := d.Stop(); err != nil {
			return err
		}
	}
	//return xhyve("unregistervm", "--delete", d.MachineName)
	return nil
}

func (d *Driver) Restart() error { // TODO
	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s == state.Running {
		if err := d.Stop(); err != nil {
			return err
		}
	}
	return d.Start()
}

func (d *Driver) Kill() error { // TODO
	//return xhyve("controlvm", d.MachineName, "poweroff")
	return nil
}

func (d *Driver) setMachineNameIfNotSet() {
	if d.MachineName == "" {
		d.MachineName = fmt.Sprintf("docker-machine-unknown")
	}
}

func (d *Driver) ISOPath() string {
	return path.Join(d.LocalArtifactPath("."), isoFilename)
}

func (d *Driver) imgPath() string {
	return path.Join(d.LocalArtifactPath("."), fmt.Sprintf("%s.img", d.MachineName))
}

func (d *Driver) userdataPath() string {
	return path.Join(d.LocalArtifactPath("."), "userdata.tar")
}

func (d *Driver) getIPfromDHCPLease() (string, error) {
	var dhcpfh *os.File
	var dhcpcontent []byte
	var macaddr string
	var err error
	var lastipmatch string
	var currentip string

	// DHCP lease table for NAT vmnet interface
	var dhcpfile = "/var/db/dhcpd_leases"

	if dhcpfh, err = os.Open(dhcpfile); err != nil {
		return "", err
	}
	defer dhcpfh.Close()

	if dhcpcontent, err = ioutil.ReadAll(dhcpfh); err != nil {
		return "", err
	}

	// Get the IP from the lease table.
	leaseip := regexp.MustCompile(`^\s*ip_address=(.+?)$`)
	log.Debugf("leaseip: %s", leaseip) // print regex code.
	// Get the MAC address associated.
	leasemac := regexp.MustCompile(`^\s*hw_address=1,(.+?)$`)
	log.Debugf("leasemac: %s", leasemac) // print regex code.

	for _, line := range strings.Split(string(dhcpcontent), "\n") {

		if matches := leaseip.FindStringSubmatch(line); matches != nil {
			log.Debugf("ip matches: %s", matches)
			lastipmatch = matches[1]
			log.Debugf("lastipmatch: %s", lastipmatch)
			continue
		}

		if matches := leasemac.FindStringSubmatch(line); matches != nil {
			log.Debugf("mac matches: %s", matches)
			currentip = lastipmatch
			macaddr = matches[1]
			log.Debug("macaddr: %s", macaddr)
			continue
		}
	}

	if currentip == "" {
		return "", fmt.Errorf("IP not found for MAC %s in DHCP leases", macaddr)
	}

	// if macaddr == "" {
	// 	return "", fmt.Errorf("couldn't find MAC address in DHCP leases file %s", dhcpfile)
	// }

	log.Debugf("IP found in DHCP lease table: %s", currentip)
	return currentip, nil
}

func (d *Driver) publicSSHKeyPath() string {
	return d.GetSSHKeyPath() + ".pub"
}

func (d *Driver) extractKernelImages() error {
	var vmlinuz64 = "/Volumes/Boot2Docker-v1.8/boot/vmlinuz64" // TODO Do not hardcode boot2docker version
	var initrd = "/Volumes/Boot2Docker-v1.8/boot/initrd.img"   // TODO Do not hardcode boot2docker version

	log.Debugf("Mounting %s", isoFilename)
	hdiutil("attach", d.ISOPath()) // TODO need parse attached disk identifier.

	log.Debugf("Extract vmlinuz64")
	if err := mcnutils.CopyFile(vmlinuz64, filepath.Join(d.LocalArtifactPath("."), "vmlinuz64")); err != nil {
		return err
	}
	log.Debugf("Extract initrd.img")
	if err := mcnutils.CopyFile(initrd, filepath.Join(d.LocalArtifactPath("."), "initrd.img")); err != nil {
		return err
	}
	log.Debugf("Unmounting %s", isoFilename)
	if err := hdiutil("unmount", "/Volumes/Boot2Docker-v1.8/"); err != nil { // TODO need eject instead unmount. It would remain in the space of /dev.
		return err
	}

	return nil
}

func (d *Driver) generateBlankDiskImage(count int) error {
	cmd := dd
	output := d.imgPath()
	cmd("/dev/zero", output, "1m", count)

	return nil
}

// Make a boot2docker userdata.tar key bundle
func (d *Driver) generateKeyBundle() error { // TODO
	magicString := "boot2docker, this is xhyve speaking"

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	// magicString first so the automount script knows to format the disk
	file := &tar.Header{Name: magicString, Size: int64(len(magicString))}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(magicString)); err != nil {
		return err
	}
	// .ssh/key.pub => authorized_keys
	file = &tar.Header{Name: ".ssh", Typeflag: tar.TypeDir, Mode: 0700}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	pubKey, err := ioutil.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys2", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	raw := buf.Bytes()

	if err := ioutil.WriteFile(d.userdataPath(), raw, 0644); err != nil {
		return err
	}

	return nil
}

package deploy

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/dan-v/dosxvpn/genconfig"

	"github.com/dan-v/dosxvpn/doclient"
	"github.com/dan-v/dosxvpn/services"
	"github.com/dan-v/dosxvpn/services/coreos"
	"github.com/dan-v/dosxvpn/services/dosxvpn"
	"github.com/dan-v/dosxvpn/services/pihole"
	"github.com/dan-v/dosxvpn/sshclient"
	"github.com/dan-v/dosxvpn/vpn"
)

const (
	DropletBaseName = "dosxvpn"
	DropletImage    = "coreos-beta"
	DropletSize     = "512mb"
)

var (
	FilepathDosxvpnConfigDir = filepath.Join(userHomeDir(), ".dosxvpn")
	FilenameAppleConfig      = "%s.apple.mobileconfig"
	FilenameAndroidConfig    = "%s.android.sswan"
	FilenamePrivateKey       = "%s.client.cert.p12"
	FilenameCACert           = "%s.ca.cert.pem"
	FilenameServerCert       = "%s.server.cert.pem"
	VpnFiles                 = map[string]string{
		"/etc/ipsec.d/client.cert.p12":       FilenamePrivateKey,
		"/etc/ipsec.d/cacerts/ca.cert.pem":   FilenameCACert,
		"/etc/ipsec.d/certs/server.cert.pem": FilenameServerCert,
	}
)

type Deployment struct {
	Region          string
	AutoConfigure   bool
	Name            string
	Token           string
	services        []services.Service
	userData        string
	dropletIP       string
	dropletID       int
	doClient        *doclient.Client
	sshClient       *sshclient.Client
	VpnPassword     string
	Status          string `json:"status"`
	VPNIPAddress    string `json:"ip_address"`
	InitialPublicIP string `json:"initial_ip"`
	FinalPublicIP   string `json:"final_ip"`
}

func New(token, region string, autoConfigure bool) (*Deployment, error) {
	deploy := &Deployment{
		Name:          DropletBaseName + "-" + randomString(3) + "-" + region,
		Token:         token,
		Region:        region,
		AutoConfigure: autoConfigure,
		doClient:      doclient.New(token),
		services: []services.Service{
			&coreos.Service{}, &dosxvpn.Service{}, &pihole.Service{},
		},
		Status: "pending auth",
	}
	var err error
	deploy.sshClient, err = sshclient.New()
	if err != nil {
		return nil, err
	}
	deploy.userData, err = services.GenerateCloudConfig(deploy.sshClient.KeyPair.AuthorizedKey, deploy.services)
	if err != nil {
		return nil, err
	}
	return deploy, nil
}

func (d *Deployment) Run() error {
	log.Println("Getting initial IP...")
	initialPublicIP, _ := getPublicIp()
	d.InitialPublicIP = initialPublicIP
	log.Println("Initial IP is", d.InitialPublicIP)

	log.Println("Creating droplet...")
	dropletID, err := d.doClient.CreateDroplet(d.Name, d.Region, DropletSize, d.userData, DropletImage)
	if err != nil {
		log.Fatal(err)
	}
	d.dropletID = dropletID
	log.Printf("Finished creating droplet %s", d.Name)

	log.Println("Waiting for droplet to get IP...")
	dropletIP, err := d.doClient.WaitForDropletIP(d.dropletID)
	if err != nil {
		log.Fatal(err)
	}
	d.dropletIP = dropletIP
	log.Printf("Droplet now has IP %s...", d.dropletIP)
	d.VPNIPAddress = d.dropletIP

	log.Println("Creating firewall...")
	err = d.doClient.CreateFirewall(d.Name, d.dropletID)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Finished creating firewall...")

	d.Status = "waiting for ssh"
	log.Println("Waiting for SSH to start...")
	err = waitForSSH(d.dropletIP)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("SSH is now online...")

	log.Println("Waiting for VPN to become active...")
	_, err = d.sshClient.Run("core", d.dropletIP, "until docker logs dosxvpn &>/dev/null; do sleep 2; done; sleep 5;")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("VPN is now active...")

	log.Println("Getting/generating VPN files...")
	privateKeyPasswordString, err := d.sshClient.GetFileFromContainer("core", d.dropletIP, "dosxvpn", "/etc/ipsec.d/client.cert.p12.password")
	if err != nil {
		return err
	}
	d.VpnPassword = privateKeyPasswordString

	privateKeyString, err := d.sshClient.GetFileFromContainer("core", d.dropletIP, "dosxvpn", "/etc/ipsec.d/client.cert.p12")
	if err != nil {
		return err
	}
	saveConfig(privateKeyString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenamePrivateKey, d.Name)))

	caCertString, err := d.sshClient.GetFileFromContainer("core", d.dropletIP, "dosxvpn", "/etc/ipsec.d/cacerts/ca.cert.pem")
	if err != nil {
		return err
	}
	saveConfig(caCertString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameCACert, d.Name)))

	serverCertString, err := d.sshClient.GetFileFromContainer("core", d.dropletIP, "dosxvpn", "/etc/ipsec.d/certs/server.cert.pem")
	if err != nil {
		return err
	}
	saveConfig(serverCertString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameServerCert, d.Name)))

	appleConfigString, err := genconfig.GenerateAppleConfig(d.dropletIP, d.Name, privateKeyPasswordString, privateKeyString, caCertString, serverCertString)
	if err != nil {
		return err
	}
	saveConfig(appleConfigString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameAppleConfig, d.Name)))

	androidConfigString, err := genconfig.GenerateAndroidConfig(d.dropletIP, d.Name, privateKeyString, caCertString)
	if err != nil {
		return err
	}
	saveConfig(androidConfigString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameAndroidConfig, d.Name)))

	log.Println("Finished getting/generating VPN files...")

	if d.AutoConfigure {
		d.Status = "adding vpn to osx"
		log.Println("Adding VPN to OSX...")
		err = vpn.OSXAddVPN(filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameAppleConfig, d.Name)))
		if err != nil {
			log.Println("Failed to add VPN to OSX.", err)
		}
		log.Println("Done Adding VPN to OSX...")

		d.Status = "waiting for ip address change"
		for j := 0; j < 10; j++ {
			time.Sleep(time.Second * 5)
			newIP, err := getPublicIp()
			if err == nil && newIP != "" && newIP != initialPublicIP {
				newIP, _ := getPublicIp()
				d.FinalPublicIP = newIP
				break
			}
		}
		d.Status = "done"
	}

	log.Println("##############################")
	log.Println("VPN IP:", d.dropletIP)
	log.Println("Apple Config:", filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameAppleConfig, d.Name)))
	log.Println("Android Config:", filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameAndroidConfig, d.Name)))
	log.Println("Client Private Key:", filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenamePrivateKey, d.Name)))
	log.Println("Client Private Key Passphrase:", privateKeyPasswordString)
	log.Println("CA Certificate:", filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameCACert, d.Name)))
	log.Println("Server Certificate:", filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(FilenameServerCert, d.Name)))
	log.Println("##############################")

	return nil
}

func (d *Deployment) getAndSaveConfigFiles(ip, name string) error {
	for remoteFile, localFile := range VpnFiles {
		fileString, err := d.sshClient.GetFileFromContainer("core", ip, "dosxvpn", remoteFile)
		if err != nil {
			return err
		}
		saveConfig(fileString, filepath.Join(FilepathDosxvpnConfigDir, fmt.Sprintf(localFile, name)))
	}
	return nil
}

func randomString(n int) string {
	rand.Seed(time.Now().UTC().UnixNano())
	return strconv.Itoa(rand.Int())[:n]
}

func saveConfig(configFileString, saveFile string) (string, error) {
	os.MkdirAll(FilepathDosxvpnConfigDir, os.ModePerm)
	err := ioutil.WriteFile(saveFile, []byte(configFileString), 0644)
	if err != nil {
		return "", err
	}
	return saveFile, nil
}

func getPublicIp() (string, error) {
	resp, err := http.Get("http://checkip.amazonaws.com/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(buf)), nil
}

func waitForPort(host string, port int) error {
	for attempt := uint(0); attempt < 15; attempt++ {
		conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		} else if err != nil {
			return err
		}
		conn.Close()
		return nil
	}
	return fmt.Errorf("timed out waiting for port %d to open", port)
}

func waitForSSH(ip string) error {
	return waitForPort(ip, 22)
}

func userHomeDir() string {
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home
	}
	return os.Getenv("HOME")
}

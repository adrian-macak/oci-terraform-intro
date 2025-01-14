package terratest

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"os/exec"

	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/gruntwork-io/terratest/modules/ssh"
	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/oracle/oci-go-sdk/common"
	"github.com/oracle/oci-go-sdk/core"
	"github.com/oracle/oci-go-sdk/identity"
)

const (
	// OCI params
	sshUserName = "opc"
	nginxName   = "nginx"
	nginxPort   = "80"
	// Terratest retries
	maxRetries          = 20
	sleepBetweenRetries = 5 * time.Second
)

var (
	options *terraform.Options
)

func terraformEnvOptions() *terraform.Options {
	return &terraform.Options{
		TerraformDir: "..",
		Vars: map[string]interface{}{
			"region":           os.Getenv("TF_VAR_region"),
			"tenancy_ocid":     os.Getenv("TF_VAR_tenancy_ocid"),
			"user_ocid":        os.Getenv("TF_VAR_user_ocid"),
			"CompartmentOCID":  os.Getenv("TF_VAR_CompartmentOCID"),
			"fingerprint":      os.Getenv("TF_VAR_fingerprint"),
			"private_key_path": os.Getenv("TF_VAR_private_key_path"),
			// "pass_phrase":      oci.GetPassPhraseFromEnvVar(),
			"ssh_public_key":  os.Getenv("TF_VAR_ssh_public_key"),
			"ssh_private_key": os.Getenv("TF_VAR_ssh_private_key"),
		},
	}
}

func TestTerraform(t *testing.T) {
	options = terraformEnvOptions()

	defer terraform.Destroy(t, options)
	// terraform.WorkspaceSelectOrNew(t, options, "terratest-vita")
	terraform.InitAndApply(t, options)

	runSubtests(t)
}

func TestWithoutProvisioning(t *testing.T) {
	options = terraformEnvOptions()

	runSubtests(t)
}

func runSubtests(t *testing.T) {
	t.Run("sshBastion", sshBastion)
	t.Run("sshWeb", sshWeb)
	t.Run("netstatNginx", netstatNginx)
	t.Run("curlWebServer", curlWebServer)
	t.Run("checkVpn", checkVpn)
	t.Run("checkGetAllAvailabilityDomains", checkGetAllAvailabilityDomains)
	t.Run("checkSubnetsCount", checkSubnetsCount)
	t.Run("checkLoadBalancerCurl", checkLoadBalancerCurl)
}

func sshBastion(t *testing.T) {
	ssh.CheckSshConnection(t, bastionHost(t))
}

func sshWeb(t *testing.T) {
	jumpSsh(t, "whoami", sshUserName, false)
}

func netstatNginx(t *testing.T) {
	netstatService(t, nginxName, nginxPort, 1)
}

func curlWebServer(t *testing.T) {
	curlService(t, "nginx", "", "80", "200")
}

func checkVpn(t *testing.T) {
	// client
	config := common.CustomProfileConfigProvider("", "CzechEdu")
	c, _ := core.NewVirtualNetworkClientWithConfigurationProvider(config)
	// c, _ := core.NewVirtualNetworkClientWithConfigurationProvider(common.DefaultConfigProvider())

	// request
	request := core.GetVcnRequest{}
	vcnId := sanitizedVcnId(t)
	request.VcnId = &vcnId

	// response
	response, err := c.GetVcn(context.Background(), request)

	if err != nil {
		t.Fatalf("error in calling vcn: %s", err.Error())
	}

	// assertions
	expected := "Web VCN-default"
	actual := response.Vcn.DisplayName

	if expected != *actual {
		t.Fatalf("wrong vcn display name: expected %q, got %q", expected, *actual)
	}

	expected = "10.0.0.0/16"
	actual = response.Vcn.CidrBlock

	if expected != *actual {
		t.Fatalf("wrong cidr block: expected %q, got %q", expected, *actual)
	}
}

func checkGetAllAvailabilityDomains(t *testing.T) {
	options = terraformEnvOptions()
	configProvider := common.DefaultConfigProvider()
	client, err := identity.NewIdentityClientWithConfigurationProvider(configProvider)
	if err != nil {
		t.Fatalf("error occured: %s", err.Error())
	}

	compartmentID := options.Vars["CompartmentOCID"].(string)

	request := identity.ListAvailabilityDomainsRequest{CompartmentId: &compartmentID}
	response, err := client.ListAvailabilityDomains(context.Background(), request)
	if err != nil {
		t.Fatalf("error in: %s", err.Error())
	}

	if len(response.Items) == 0 {
		t.Fatalf("No availability domains found in the %s compartment", compartmentID)
	}

	avs := strings.Join(availabilityDomainsNames(response.Items), " ")
	t.Log("AVs: " + avs)

	// assertions
	expected := "NoND:EU-FRANKFURT-1-AD-3"

	if !strings.Contains(avs, expected) {
		t.Fatalf("missing expected availability domain %q", expected)
	}
}

func availabilityDomainsNames(ads []identity.AvailabilityDomain) []string {
	names := []string{}
	for _, ad := range ads {
		names = append(names, *ad.Name)
	}
	return names
}

func checkSubnetsCount(t *testing.T) {
	options = terraformEnvOptions()
	configProvider := common.DefaultConfigProvider()
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(configProvider)
	if err != nil {
		t.Fatalf("error occured: %s", err.Error())
	}

	compartmentID := options.Vars["CompartmentOCID"].(string)
	vcnIDs, err := GetAllVcnIDsE(t, compartmentID)
	if err != nil {
		t.Fatalf("error occured: %s", err.Error())
	}

	for _, vcnID := range vcnIDs {
		request := core.ListSubnetsRequest{
			CompartmentId: &compartmentID,
			VcnId:         &vcnID,
		}
		response, err := client.ListSubnets(context.Background(), request)
		if err != nil {
			t.Fatalf("error occured: %s", err.Error())
		}

		// assertions
		expected := 3
		t.Logf(vcnID+", subnets count: %i", len(response.Items))
		if len(response.Items) != expected {
			t.Fatalf("Wrong number of subnets")
		}
	}
}

// GetAllVcnIDsE gets the list of VCNs available in the given compartment.
func GetAllVcnIDsE(t *testing.T, compartmentID string) ([]string, error) {
	configProvider := common.DefaultConfigProvider()
	client, err := core.NewVirtualNetworkClientWithConfigurationProvider(configProvider)
	if err != nil {
		return nil, err
	}

	request := core.ListVcnsRequest{CompartmentId: &compartmentID}
	response, err := client.ListVcns(context.Background(), request)
	if err != nil {
		return nil, err
	}

	if len(response.Items) == 0 {
		return nil, fmt.Errorf("No VCNs found in the %s compartment", compartmentID)
	}

	ids := []string{}
	for _, vcn := range response.Items {
		ids = append(ids, *vcn.Id)
	}
	return ids, nil
}

func checkLoadBalancerCurl(t *testing.T) {
	lb_address := terraform.OutputList(t, options, "lb_ip")[0]

	for i := 0; i < 10; i++ {
		result, err := exec.Command("curl", "http://"+lb_address).Output()
		response := string(result)
		if err != nil {
			t.Fatalf("error occured: %s", err.Error())
		}

		// assertions
		expected := "web0"
		if !strings.Contains(response, expected) {
			t.Fatalf("Different name contained.")
		}
	}
}

func sanitizedVcnId(t *testing.T) string {
	raw := terraform.Output(t, options, "VcnID")
	return strings.Split(raw, "\"")[1]
}

// ~~~~~~~~~~~~~~~~ Helper functions ~~~~~~~~~~~~~~~~

func bastionHost(t *testing.T) ssh.Host {
	bastionIP := terraform.OutputList(t, options, "BastionPublicIP")[0]
	return sshHost(t, bastionIP)
}

func webHost(t *testing.T) ssh.Host {
	webIP := terraform.OutputList(t, options, "WebServerPrivateIPs")[0]
	return sshHost(t, webIP)
}

func sshHost(t *testing.T, ip string) ssh.Host {
	return ssh.Host{
		Hostname:    ip,
		SshUserName: sshUserName,
		SshKeyPair:  loadKeyPair(t),
	}
}

func curlService(t *testing.T, serviceName string, path string, port string, returnCode string) {
	bastionHost := bastionHost(t)
	webIPs := webServerIPs(t)

	for _, cp := range webIPs {
		re := strings.NewReplacer("[", "", "]", "")
		host := re.Replace(cp)
		command := curl(host, port, path)
		description := fmt.Sprintf("curl to %s on %s:%s%s", serviceName, cp, port, path)

		out := retry.DoWithRetry(t, description, maxRetries, sleepBetweenRetries, func() (string, error) {
			out, err := ssh.CheckSshCommandE(t, bastionHost, command)
			if err != nil {
				return "", err
			}

			out = strings.TrimSpace(out)
			return out, nil
		})

		if out != returnCode {
			t.Fatalf("%s on %s: expected %q, got %q", serviceName, cp, returnCode, out)
		}
	}
}

func curl(host string, port string, path string) string {
	return fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' http://%s:%s%s", host, port, path)
}

func webServerIPs(t *testing.T) []string {
	return terraform.OutputList(t, options, "WebServerPrivateIPs")
}

func jumpSsh(t *testing.T, command string, expected string, retryAssert bool) string {
	bastionHost := bastionHost(t)
	webHost := webHost(t)
	description := fmt.Sprintf("ssh jump to %q with command %q", webHost.Hostname, command)

	out := retry.DoWithRetry(t, description, maxRetries, sleepBetweenRetries, func() (string, error) {
		out, err := ssh.CheckPrivateSshConnectionE(t, bastionHost, webHost, command)
		if err != nil {
			return "", err
		}

		out = strings.TrimSpace(out)
		if retryAssert && out != expected {
			return "", fmt.Errorf("assert with retry: expected %q, got %q", expected, out)
		}
		return out, nil
	})

	if out != expected {
		t.Fatalf("command %q on %s: expected %q, got %q", command, webHost.Hostname, expected, out)
	}

	return out
}

func loadKeyPair(t *testing.T) *ssh.KeyPair {
	publicKeyPath := options.Vars["ssh_public_key"].(string)
	publicKey, err := ioutil.ReadFile(publicKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	privateKeyPath := options.Vars["ssh_private_key"].(string)
	privateKey, err := ioutil.ReadFile(privateKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	return &ssh.KeyPair{
		PublicKey:  string(publicKey),
		PrivateKey: string(privateKey),
	}
}

func netstatService(t *testing.T, service string, port string, expectedCount int) {
	command := fmt.Sprintf("sudo netstat -tnlp | grep '%s' | grep ':%s' | wc -l", service, port)
	jumpSsh(t, command, strconv.Itoa(expectedCount), true)
}

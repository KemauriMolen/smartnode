package rocketpool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	osUser "os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a8m/envsubst"
	"github.com/fatih/color"
	"github.com/urfave/cli"
	"golang.org/x/crypto/ssh"
	kh "golang.org/x/crypto/ssh/knownhosts"
	"gopkg.in/yaml.v2"

	"github.com/alessio/shellescape"
	"github.com/blang/semver/v4"
	externalip "github.com/glendc/go-external-ip"
	"github.com/mitchellh/go-homedir"
	"github.com/rocket-pool/smartnode/shared"
	"github.com/rocket-pool/smartnode/shared/services/config"
	"github.com/rocket-pool/smartnode/shared/utils/net"
)

// Config
const (
	InstallerURL     string = "https://github.com/rocket-pool/smartnode-install/releases/download/%s/install.sh"
	UpdateTrackerURL string = "https://github.com/rocket-pool/smartnode-install/releases/download/%s/install-update-tracker.sh"

	SettingsFile             string = "user-settings.yml"
	PrometheusConfigTemplate string = "prometheus.tmpl"
	PrometheusFile           string = "prometheus.yml"

	APIContainerSuffix string = "_api"
	APIBinPath         string = "/go/bin/rocketpool"

	templatesDir string = "templates"
	overrideDir  string = "override"
	runtimeDir   string = "runtime"

	templateSuffix    string = ".tmpl"
	composeFileSuffix string = ".yml"

	DebugColor = color.FgYellow
)

// Rocket Pool client
type Client struct {
	configPath         string
	daemonPath         string
	maxFee             float64
	maxPrioFee         float64
	gasLimit           uint64
	customNonce        *big.Int
	client             *ssh.Client
	originalMaxFee     float64
	originalMaxPrioFee float64
	originalGasLimit   uint64
	debugPrint         bool
}

// Create new Rocket Pool client from CLI context
func NewClientFromCtx(c *cli.Context) (*Client, error) {
	return NewClient(c.GlobalString("config-path"),
		c.GlobalString("daemon-path"),
		c.GlobalString("host"),
		c.GlobalString("user"),
		c.GlobalString("key"),
		c.GlobalString("passphrase"),
		c.GlobalString("known-hosts"),
		c.GlobalFloat64("maxFee"),
		c.GlobalFloat64("maxPrioFee"),
		c.GlobalUint64("gasLimit"),
		c.GlobalString("nonce"),
		c.GlobalBool("debug"))
}

// Create new Rocket Pool client
func NewClient(configPath string, daemonPath string, hostAddress string, user string, keyPath string, passphrasePath string, knownhostsFile string, maxFee float64, maxPrioFee float64, gasLimit uint64, customNonce string, debug bool) (*Client, error) {

	// Initialize SSH client if configured for SSH
	var sshClient *ssh.Client
	if hostAddress != "" {

		// Check parameters
		if user == "" {
			return nil, errors.New("The SSH user (--user) must be specified.")
		}
		if keyPath == "" {
			return nil, errors.New("The SSH private key path (--key) must be specified.")
		}

		// Read private key
		keyBytes, err := ioutil.ReadFile(os.ExpandEnv(keyPath))
		if err != nil {
			return nil, fmt.Errorf("Could not read SSH private key at %s: %w", keyPath, err)
		}

		// Read passphrase
		var passphrase []byte
		if passphrasePath != "" {
			passphrase, err = ioutil.ReadFile(os.ExpandEnv(passphrasePath))
			if err != nil {
				return nil, fmt.Errorf("Could not read SSH passphrase at %s: %w", passphrasePath, err)
			}
		}

		// Parse private key
		var key ssh.Signer
		if passphrase == nil {
			key, err = ssh.ParsePrivateKey(keyBytes)
		} else {
			key, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, passphrase)
		}
		if err != nil {
			return nil, fmt.Errorf("Could not parse SSH private key at %s: %w", keyPath, err)
		}

		// Prepare the server host key callback function
		if knownhostsFile == "" {
			// Default to using the current users known_hosts file if one wasn't provided
			usr, err := osUser.Current()
			if err != nil {
				return nil, fmt.Errorf("Could not get current user: %w", err)
			}
			knownhostsFile = fmt.Sprintf("%s/.ssh/known_hosts", usr.HomeDir)
		}

		hostKeyCallback, err := kh.New(knownhostsFile)
		if err != nil {
			return nil, fmt.Errorf("Could not create hostKeyCallback function: %w", err)
		}

		// Initialise client
		sshClient, err = ssh.Dial("tcp", net.DefaultPort(hostAddress, "22"), &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
			HostKeyCallback: hostKeyCallback,
		})
		if err != nil {
			return nil, fmt.Errorf("Could not connect to %s as %s: %w", hostAddress, user, err)
		}

	}

	var customNonceBigInt *big.Int = nil
	var success bool
	if customNonce != "" {
		customNonceBigInt, success = big.NewInt(0).SetString(customNonce, 0)
		if !success {
			return nil, fmt.Errorf("Invalid nonce: %s", customNonce)
		}
	}

	// Return client
	return &Client{
		configPath:         os.ExpandEnv(configPath),
		daemonPath:         os.ExpandEnv(daemonPath),
		maxFee:             maxFee,
		maxPrioFee:         maxPrioFee,
		gasLimit:           gasLimit,
		originalMaxFee:     maxFee,
		originalMaxPrioFee: maxPrioFee,
		originalGasLimit:   gasLimit,
		customNonce:        customNonceBigInt,
		client:             sshClient,
		debugPrint:         debug,
	}, nil

}

// Close client remote connection
func (c *Client) Close() {
	if c.client != nil {
		_ = c.client.Close()
	}
}

// Load the config
func (c *Client) LoadConfig() (*config.RocketPoolConfig, bool, error) {
	cfg, err := c.loadConfig(fmt.Sprintf("%s/%s", c.configPath, SettingsFile))
	if err != nil {
		return nil, false, err
	}

	isNew := false
	if cfg == nil {
		cfg = config.NewRocketPoolConfig()
		isNew = true
	}
	return cfg, isNew, nil
}

// Save the config
func (c *Client) SaveConfig(cfg *config.RocketPoolConfig) error {
	return c.saveConfig(cfg, fmt.Sprintf("%s/%s", c.configPath, SettingsFile))
}

// Load the Prometheus template, do an environment variable substitution, and save it
func (c *Client) UpdatePrometheusConfiguration(settings map[string]string) error {
	prometheusTemplatePath, err := homedir.Expand(fmt.Sprintf("%s/%s", c.configPath, PrometheusConfigTemplate))
	if err != nil {
		return fmt.Errorf("Error expanding Prometheus template path: %w", err)
	}

	prometheusConfigPath, err := homedir.Expand(fmt.Sprintf("%s/%s", c.configPath, PrometheusFile))
	if err != nil {
		return fmt.Errorf("Error expanding Prometheus config file path: %w", err)
	}

	// Set the environment variables defined in the user settings for metrics
	oldValues := map[string]string{}
	for varName, varValue := range settings {
		oldValues[varName] = os.Getenv(varName)
		os.Setenv(varName, varValue)
	}

	// Read and substitute the template
	contents, err := envsubst.ReadFile(prometheusTemplatePath)
	if err != nil {
		return fmt.Errorf("Error reading and substituting Prometheus configuration template: %w", err)
	}

	// Unset the env vars
	for name, value := range oldValues {
		os.Setenv(name, value)
	}

	// Write the actual Prometheus config file
	err = ioutil.WriteFile(prometheusConfigPath, contents, 0664)
	if err != nil {
		return fmt.Errorf("Could not write Prometheus config file to %s: %w", shellescape.Quote(prometheusConfigPath), err)
	}
	err = os.Chmod(prometheusConfigPath, 0664)
	if err != nil {
		return fmt.Errorf("Could not set Prometheus config file permissions: %w", shellescape.Quote(prometheusConfigPath), err)
	}

	return nil
}

// Install the Rocket Pool service
func (c *Client) InstallService(verbose, noDeps bool, network, version, path string) error {

	// Get installation script downloader type
	downloader, err := c.getDownloader()
	if err != nil {
		return err
	}

	// Get installation script flags
	flags := []string{
		"-n", fmt.Sprintf("%s", shellescape.Quote(network)),
		"-v", fmt.Sprintf("%s", shellescape.Quote(version)),
	}
	if path != "" {
		flags = append(flags, fmt.Sprintf("-p %s", shellescape.Quote(path)))
	}
	if noDeps {
		flags = append(flags, "-d")
	}

	// Initialize installation command
	cmd, err := c.newCommand(fmt.Sprintf("%s %s | sh -s -- %s", downloader, fmt.Sprintf(InstallerURL, version), strings.Join(flags, " ")))
	if err != nil {
		return err
	}
	defer func() {
		_ = cmd.Close()
	}()

	// Get command output pipes
	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmdErr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Print progress from stdout
	go (func() {
		scanner := bufio.NewScanner(cmdOut)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	})()

	// Read command & error output from stderr; render in verbose mode
	var errMessage string
	go (func() {
		c := color.New(DebugColor)
		scanner := bufio.NewScanner(cmdErr)
		for scanner.Scan() {
			errMessage = scanner.Text()
			if verbose {
				_, _ = c.Println(scanner.Text())
			}
		}
	})()

	// Run command and return error output
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("Could not install Rocket Pool service: %s", errMessage)
	}
	return nil

}

// Install the update tracker
func (c *Client) InstallUpdateTracker(verbose bool, version string) error {

	// Get installation script downloader type
	downloader, err := c.getDownloader()
	if err != nil {
		return err
	}

	// Get installation script flags
	flags := []string{
		"-v", fmt.Sprintf("%s", shellescape.Quote(version)),
	}

	// Initialize installation command
	cmd, err := c.newCommand(fmt.Sprintf("%s %s | sh -s -- %s", downloader, fmt.Sprintf(UpdateTrackerURL, version), strings.Join(flags, " ")))
	if err != nil {
		return err
	}
	defer func() {
		_ = cmd.Close()
	}()

	// Get command output pipes
	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmdErr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Print progress from stdout
	go (func() {
		scanner := bufio.NewScanner(cmdOut)
		for scanner.Scan() {
			fmt.Println(scanner.Text())
		}
	})()

	// Read command & error output from stderr; render in verbose mode
	var errMessage string
	go (func() {
		c := color.New(DebugColor)
		scanner := bufio.NewScanner(cmdErr)
		for scanner.Scan() {
			errMessage = scanner.Text()
			if verbose {
				_, _ = c.Println(scanner.Text())
			}
		}
	})()

	// Run command and return error output
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("Could not install Rocket Pool update tracker: %s", errMessage)
	}
	return nil

}

// Start the Rocket Pool service
func (c *Client) StartService(composeFiles []string) error {
	cmd, err := c.compose(composeFiles, "up -d")
	if err != nil {
		return err
	}
	return c.printOutput(cmd)
}

// Pause the Rocket Pool service
func (c *Client) PauseService(composeFiles []string) error {
	cmd, err := c.compose(composeFiles, "stop")
	if err != nil {
		return err
	}
	return c.printOutput(cmd)
}

// Stop the Rocket Pool service
func (c *Client) StopService(composeFiles []string) error {
	cmd, err := c.compose(composeFiles, "down -v")
	if err != nil {
		return err
	}
	return c.printOutput(cmd)
}

// Print the Rocket Pool service status
func (c *Client) PrintServiceStatus(composeFiles []string) error {
	cmd, err := c.compose(composeFiles, "ps")
	if err != nil {
		return err
	}
	return c.printOutput(cmd)
}

// Print the Rocket Pool service logs
func (c *Client) PrintServiceLogs(composeFiles []string, tail string, serviceNames ...string) error {
	sanitizedStrings := make([]string, len(serviceNames))
	for i, serviceName := range serviceNames {
		sanitizedStrings[i] = fmt.Sprintf("%s", shellescape.Quote(serviceName))
	}
	cmd, err := c.compose(composeFiles, fmt.Sprintf("logs -f --tail %s %s", shellescape.Quote(tail), strings.Join(sanitizedStrings, " ")))
	if err != nil {
		return err
	}
	return c.printOutput(cmd)
}

// Print the Rocket Pool service stats
func (c *Client) PrintServiceStats(composeFiles []string) error {

	// Get service container IDs
	cmd, err := c.compose(composeFiles, "ps -q")
	if err != nil {
		return err
	}
	containers, err := c.readOutput(cmd)
	if err != nil {
		return err
	}
	containerIds := strings.Split(strings.TrimSpace(string(containers)), "\n")

	// Print stats
	return c.printOutput(fmt.Sprintf("docker stats %s", strings.Join(containerIds, " ")))

}

// Get the Rocket Pool service version
func (c *Client) GetServiceVersion() (string, error) {

	// Get service container version output
	var cmd string
	if c.daemonPath == "" {
		containerName, err := c.getAPIContainerName()
		if err != nil {
			return "", err
		}
		cmd = fmt.Sprintf("docker exec %s %s --version", shellescape.Quote(containerName), shellescape.Quote(APIBinPath))
	} else {
		cmd = fmt.Sprintf("%s --version", shellescape.Quote(c.daemonPath))
	}
	versionBytes, err := c.readOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("Could not get Rocket Pool service version: %w", err)
	}

	// Get the version string
	outputString := string(versionBytes)
	elements := strings.Fields(outputString) // Split on whitespace
	if len(elements) < 1 {
		return "", fmt.Errorf("Could not parse Rocket Pool service version number from output '%s'", outputString)
	}
	versionString := elements[len(elements)-1]

	// Make sure it's a semantic version
	version, err := semver.Make(versionString)
	if err != nil {
		return "", fmt.Errorf("Could not parse Rocket Pool service version number from output '%s': %w", outputString, err)
	}

	// Return the parsed semantic version (extra safety)
	return version.String(), nil

}

// Increments the custom nonce parameter.
// This is used for calls that involve multiple transactions, so they don't all have the same nonce.
func (c *Client) IncrementCustomNonce() {
	c.customNonce.Add(c.customNonce, big.NewInt(1))
}

// Get the current Docker image used by the given container
func (c *Client) GetDockerImage(container string) (string, error) {

	cmd := fmt.Sprintf("docker container inspect --format={{.Config.Image}} %s", container)
	image, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(image)), nil

}

// Get the current Docker image used by the given container
func (c *Client) GetDockerStatus(container string) (string, error) {

	cmd := fmt.Sprintf("docker container inspect --format={{.State.Status}} %s", container)
	status, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(status)), nil

}

// Get the time that the given container shut down
func (c *Client) GetDockerContainerShutdownTime(container string) (time.Time, error) {

	cmd := fmt.Sprintf("docker container inspect --format={{.State.FinishedAt}} %s", container)
	finishTimeBytes, err := c.readOutput(cmd)
	if err != nil {
		return time.Time{}, err
	}

	finishTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(finishTimeBytes)))
	if err != nil {
		return time.Time{}, fmt.Errorf("Error parsing validator container exit time [%s]: %w", string(finishTimeBytes), err)
	}

	return finishTime, nil

}

// Shut down a container
func (c *Client) StopContainer(container string) (string, error) {

	cmd := fmt.Sprintf("docker stop %s", container)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil

}

// Start a container
func (c *Client) StartContainer(container string) (string, error) {

	cmd := fmt.Sprintf("docker start %s", container)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil

}

// Deletes a container
func (c *Client) RemoveContainer(container string) (string, error) {

	cmd := fmt.Sprintf("docker rm %s", container)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil

}

// Deletes a container
func (c *Client) DeleteVolume(volume string) (string, error) {

	cmd := fmt.Sprintf("docker volume rm %s", volume)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil

}

// Gets the absolute file path of the client volume
func (c *Client) GetClientVolumeSource(container string) (string, error) {

	cmd := fmt.Sprintf("docker container inspect --format='{{range .Mounts}}{{if eq \"/ethclient\" .Destination}}{{.Source}}{{end}}{{end}}' %s", container)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// Gets the name of the client volume
func (c *Client) GetClientVolumeName(container string) (string, error) {

	cmd := fmt.Sprintf("docker container inspect --format='{{range .Mounts}}{{if eq \"/ethclient\" .Destination}}{{.Name}}{{end}}{{end}}' %s", container)
	output, err := c.readOutput(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// Runs the prune provisioner
func (c *Client) RunPruneProvisioner(container string, volume string, image string) error {

	// Run the prune provisioner
	cmd := fmt.Sprintf("docker run --name %s -v %s:/ethclient %s", container, volume, image)
	output, err := c.readOutput(cmd)
	if err != nil {
		return err
	}

	outputString := strings.TrimSpace(string(output))
	if outputString != "" {
		return fmt.Errorf("Unexpected output running the prune provisioner: %s", outputString)
	}

	// Remove the prune provisioner, ignoring output
	cmd = fmt.Sprintf("docker container rm %s", container)
	c.readOutput(cmd)
	return nil

}

// Get the gas settings
func (c *Client) GetGasSettings() (float64, float64, uint64) {
	return c.maxFee, c.maxPrioFee, c.gasLimit
}

// Get the gas fees
func (c *Client) AssignGasSettings(maxFee float64, maxPrioFee float64, gasLimit uint64) {
	c.maxFee = maxFee
	c.maxPrioFee = maxPrioFee
	c.gasLimit = gasLimit
}

// Load a config file
func (c *Client) loadConfig(path string) (*config.RocketPoolConfig, error) {
	expandedPath, err := homedir.Expand(path)
	if err != nil {
		return nil, err
	}
	return config.LoadFromFile(expandedPath)
}

// Save a config file
func (c *Client) saveConfig(cfg *config.RocketPoolConfig, path string) error {
	expandedPath, err := homedir.Expand(path)
	if err != nil {
		return err
	}

	settings := cfg.Serialize()
	configBytes, err := yaml.Marshal(settings)
	if err != nil {
		return fmt.Errorf("could not serialize settings file: %w", err)
	}

	if err := ioutil.WriteFile(expandedPath, configBytes, 0664); err != nil {
		return fmt.Errorf("could not write Rocket Pool config to %s: %w", shellescape.Quote(expandedPath), err)
	}
	return nil
}

// Build a docker-compose command
func (c *Client) compose(composeFiles []string, args string) (string, error) {

	// Cancel if running in non-docker mode
	if c.daemonPath != "" {
		return "", errors.New("command unavailable in Native Mode (with '--daemon-path' option specified)")
	}

	// Get the expanded config path
	expandedConfigPath, err := homedir.Expand(c.configPath)
	if err != nil {
		return "", err
	}

	// Load config
	cfg, isNew, err := c.LoadConfig()
	if err != nil {
		return "", err
	}

	if isNew {
		return "", fmt.Errorf("Settings file not found. Please run `rocketpool service config` to set up your Smartnode before starting it.")
	}

	// Check config
	if cfg.ExecutionClientMode.Value.(config.Mode) == config.Mode_Unknown {
		return "", fmt.Errorf("You haven't selected local or external mode for your Execution (ETH1) client.\nPlease run 'rocketpool service config' before running this command.")
	} else if cfg.ExecutionClientMode.Value.(config.Mode) == config.Mode_Local && cfg.ExecutionClient.Value.(config.ExecutionClient) == config.ExecutionClient_Unknown {
		return "", errors.New("No Execution (ETH1) client selected. Please run 'rocketpool service config' before running this command.")
	}
	if cfg.ConsensusClientMode.Value.(config.Mode) == config.Mode_Unknown {
		return "", fmt.Errorf("You haven't selected local or external mode for your Consensus (ETH2) client.\nPlease run 'rocketpool service config' before running this command.")
	} else if cfg.ConsensusClientMode.Value.(config.Mode) == config.Mode_Local && cfg.ConsensusClient.Value.(config.ConsensusClient) == config.ConsensusClient_Unknown {
		return "", errors.New("No Consensus (ETH2) client selected. Please run 'rocketpool service config' before running this command.")
	}

	// Make sure the selected CC is compatible with the selected EC
	consensusClientString := fmt.Sprint(cfg.ConsensusClient.Value.(config.ConsensusClient))
	_, badClients := cfg.GetCompatibleConsensusClients()
	for _, badClient := range badClients {
		if consensusClientString == badClient {
			return "", fmt.Errorf("Consensus client [%s] is incompatible with your selected Execution for Fallback Execution client choice.\nPlease run 'rocketpool service config' and select compatible clients.", consensusClientString)
		}
	}

	// Get the external IP address
	var externalIP string
	consensus := externalip.DefaultConsensus(nil, nil)
	ip, err := consensus.ExternalIP()
	if err != nil {
		fmt.Println("Warning: couldn't get external IP address; if you're using Nimbus, it may have trouble finding peers:")
		fmt.Println(err.Error())
	} else {
		externalIP = ip.String()
	}

	// Set up environment variables and deploy the template config files
	settings := cfg.GenerateEnvironmentVariables()
	settings["EXTERNAL_IP"] = shellescape.Quote(externalIP)
	settings["ROCKET_POOL_VERSION"] = shellescape.Quote(shared.RocketPoolVersion)

	// Deploy the templates and run environment variable substitution on them
	deployedContainers, err := c.deployTemplates(cfg, expandedConfigPath, settings)
	if err != nil {
		return "", fmt.Errorf("error deploying Docker templates: %w", err)
	}

	// Set up all of the environment variables to pass to the run command
	env := []string{}
	for key, value := range settings {
		env = append(env, fmt.Sprintf("%s=%s", key, shellescape.Quote(value)))
	}

	// Include all of the relevant docker compose definition files
	composeFileFlags := []string{}
	for _, container := range deployedContainers {
		composeFileFlags = append(composeFileFlags, fmt.Sprintf("-f %s", shellescape.Quote(container)))
	}

	// Return command
	return fmt.Sprintf("%s docker-compose --project-directory %s %s %s", strings.Join(env, " "), shellescape.Quote(expandedConfigPath), strings.Join(composeFileFlags, " "), args), nil

}

// Deploys all of the appropriate docker-compose template files and provisions them based on the provided configuration
func (c *Client) deployTemplates(cfg *config.RocketPoolConfig, rocketpoolDir string, settings map[string]string) ([]string, error) {

	// Check for the folders
	runtimeFolder := filepath.Join(rocketpoolDir, runtimeDir)
	_, err := os.Stat(runtimeFolder)
	if os.IsNotExist(err) {
		return []string{}, fmt.Errorf("runtime folder [%s] does not exist", runtimeFolder)
	}
	templatesFolder := filepath.Join(rocketpoolDir, templatesDir)
	_, err = os.Stat(templatesFolder)
	if os.IsNotExist(err) {
		return []string{}, fmt.Errorf("templates folder [%s] does not exist", templatesFolder)
	}
	overrideFolder := filepath.Join(rocketpoolDir, overrideDir)
	_, err = os.Stat(overrideFolder)
	if os.IsNotExist(err) {
		return []string{}, fmt.Errorf("override folder [%s] does not exist", overrideFolder)
	}

	// Clear out the runtime folder and remake it
	err = os.RemoveAll(runtimeFolder)
	if err != nil {
		return []string{}, fmt.Errorf("error deleting runtime folder [%s]: %w", runtimeFolder, err)
	}
	err = os.Mkdir(runtimeFolder, 0775)
	if err != nil {
		return []string{}, fmt.Errorf("error creating runtime folder [%s]: %w", runtimeFolder, err)
	}

	// Set the environment variables for substitution
	oldValues := map[string]string{}
	for varName, varValue := range settings {
		oldValues[varName] = os.Getenv(varName)
		os.Setenv(varName, varValue)
	}
	defer func() {
		// Unset the env vars
		for name, value := range oldValues {
			os.Setenv(name, value)
		}
	}()

	// Read and substitute the templates
	deployedContainers := []string{}

	// API
	contents, err := envsubst.ReadFile(filepath.Join(templatesFolder, config.ApiContainerName+templateSuffix))
	if err != nil {
		return []string{}, fmt.Errorf("error reading and substituting API container template: %w", err)
	}
	apiComposePath := filepath.Join(runtimeFolder, config.ApiContainerName+composeFileSuffix)
	err = ioutil.WriteFile(apiComposePath, contents, 0664)
	if err != nil {
		return []string{}, fmt.Errorf("could not write API container file to %s: %w", apiComposePath, err)
	}
	deployedContainers = append(deployedContainers, apiComposePath)
	deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.ApiContainerName+composeFileSuffix))

	// Node
	contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.NodeContainerName+templateSuffix))
	if err != nil {
		return []string{}, fmt.Errorf("error reading and substituting node container template: %w", err)
	}
	nodeComposePath := filepath.Join(runtimeFolder, config.NodeContainerName+composeFileSuffix)
	err = ioutil.WriteFile(nodeComposePath, contents, 0664)
	if err != nil {
		return []string{}, fmt.Errorf("could not write node container file to %s: %w", nodeComposePath, err)
	}
	deployedContainers = append(deployedContainers, nodeComposePath)
	deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.NodeContainerName+composeFileSuffix))

	// Watchtower
	contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.WatchtowerContainerName+templateSuffix))
	if err != nil {
		return []string{}, fmt.Errorf("error reading and substituting watchtower container template: %w", err)
	}
	watchtowerComposePath := filepath.Join(runtimeFolder, config.WatchtowerContainerName+composeFileSuffix)
	err = ioutil.WriteFile(watchtowerComposePath, contents, 0664)
	if err != nil {
		return []string{}, fmt.Errorf("could not write watchtower container file to %s: %w", watchtowerComposePath, err)
	}
	deployedContainers = append(deployedContainers, watchtowerComposePath)
	deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.WatchtowerContainerName+composeFileSuffix))

	// Validator
	contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.ValidatorContainerName+templateSuffix))
	if err != nil {
		return []string{}, fmt.Errorf("error reading and substituting validator container template: %w", err)
	}
	validatorComposePath := filepath.Join(runtimeFolder, config.ValidatorContainerName+composeFileSuffix)
	err = ioutil.WriteFile(validatorComposePath, contents, 0664)
	if err != nil {
		return []string{}, fmt.Errorf("could not write validator container file to %s: %w", validatorComposePath, err)
	}
	deployedContainers = append(deployedContainers, validatorComposePath)
	deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.ValidatorContainerName+composeFileSuffix))

	// Check the EC mode to see if it needs to be deployed
	if cfg.ExecutionClientMode.Value.(config.Mode) == config.Mode_Local {
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.Eth1ContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting execution client container template: %w", err)
		}
		eth1ComposePath := filepath.Join(runtimeFolder, config.Eth1ContainerName+composeFileSuffix)
		err = ioutil.WriteFile(eth1ComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write execution client container file to %s: %w", eth1ComposePath, err)
		}
		deployedContainers = append(deployedContainers, eth1ComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.Eth1ContainerName+composeFileSuffix))
	}

	// Check the Fallback EC mode
	if cfg.UseFallbackExecutionClient.Value == true && cfg.FallbackExecutionClientMode.Value.(config.Mode) == config.Mode_Local {
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.Eth1FallbackContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting fallback execution client container template: %w", err)
		}
		eth1FallbackComposePath := filepath.Join(runtimeFolder, config.Eth1FallbackContainerName+composeFileSuffix)
		err = ioutil.WriteFile(eth1FallbackComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write fallback execution client container file to %s: %w", eth1FallbackComposePath, err)
		}
		deployedContainers = append(deployedContainers, eth1FallbackComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.Eth1FallbackContainerName+composeFileSuffix))
	}

	// Check the Consensus mode
	if cfg.ConsensusClientMode.Value.(config.Mode) == config.Mode_Local {
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.Eth2ContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting consensus client container template: %w", err)
		}
		eth2ComposePath := filepath.Join(runtimeFolder, config.Eth2ContainerName+composeFileSuffix)
		err = ioutil.WriteFile(eth2ComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write consensus client container file to %s: %w", eth2ComposePath, err)
		}
		deployedContainers = append(deployedContainers, eth2ComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.Eth2ContainerName+composeFileSuffix))
	}

	// Check the metrics containers
	if cfg.EnableMetrics.Value == true {
		// Grafana
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.GrafanaContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting Grafana container template: %w", err)
		}
		grafanaComposePath := filepath.Join(runtimeFolder, config.GrafanaContainerName+composeFileSuffix)
		err = ioutil.WriteFile(grafanaComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write Grafana container file to %s: %w", grafanaComposePath, err)
		}
		deployedContainers = append(deployedContainers, grafanaComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.GrafanaContainerName+composeFileSuffix))

		// Node exporter
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.ExporterContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting Node Exporter container template: %w", err)
		}
		exporterComposePath := filepath.Join(runtimeFolder, config.ExporterContainerName+composeFileSuffix)
		err = ioutil.WriteFile(exporterComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write Node Exporter container file to %s: %w", exporterComposePath, err)
		}
		deployedContainers = append(deployedContainers, exporterComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.ExporterContainerName+composeFileSuffix))

		// Prometheus
		contents, err = envsubst.ReadFile(filepath.Join(templatesFolder, config.PrometheusContainerName+templateSuffix))
		if err != nil {
			return []string{}, fmt.Errorf("error reading and substituting Prometheus container template: %w", err)
		}
		prometheusComposePath := filepath.Join(runtimeFolder, config.PrometheusContainerName+composeFileSuffix)
		err = ioutil.WriteFile(prometheusComposePath, contents, 0664)
		if err != nil {
			return []string{}, fmt.Errorf("could not write Prometheus container file to %s: %w", prometheusComposePath, err)
		}
		deployedContainers = append(deployedContainers, prometheusComposePath)
		deployedContainers = append(deployedContainers, filepath.Join(overrideFolder, config.PrometheusContainerName+composeFileSuffix))
	}

	return deployedContainers, nil

}

// Call the Rocket Pool API
func (c *Client) callAPI(args string, otherArgs ...string) ([]byte, error) {
	// Sanitize arguments
	var sanitizedArgs []string
	for _, arg := range strings.Fields(args) {
		sanitizedArg := shellescape.Quote(arg)
		sanitizedArgs = append(sanitizedArgs, sanitizedArg)
	}
	args = strings.Join(sanitizedArgs, " ")
	if len(otherArgs) > 0 {
		for _, arg := range otherArgs {
			sanitizedArg := shellescape.Quote(arg)
			args += fmt.Sprintf(" %s", sanitizedArg)
		}
	}

	// Run the command
	var cmd string
	if c.daemonPath == "" {
		containerName, err := c.getAPIContainerName()
		if err != nil {
			return []byte{}, err
		}
		cmd = fmt.Sprintf("docker exec %s %s %s %s api %s", shellescape.Quote(containerName), shellescape.Quote(APIBinPath), c.getGasOpts(), c.getCustomNonce(), args)
	} else {
		cmd = fmt.Sprintf("%s --settings %s %s %s api %s",
			c.daemonPath,
			shellescape.Quote(fmt.Sprintf("%s/%s", c.configPath, SettingsFile)),
			c.getGasOpts(),
			c.getCustomNonce(),
			args)
	}

	if c.debugPrint {
		fmt.Println("To API:")
		fmt.Println(cmd)
	}

	output, err := c.readOutput(cmd)

	if c.debugPrint {
		if output != nil {
			fmt.Println("API Out:")
			fmt.Println(string(output))
		}
		if err != nil {
			fmt.Println("API Err:")
			fmt.Println(err.Error())
		}
	}

	// Reset the gas settings after the call
	c.maxFee = c.originalMaxFee
	c.maxPrioFee = c.originalMaxPrioFee
	c.gasLimit = c.originalGasLimit

	return output, err
}

// Get the API container name
func (c *Client) getAPIContainerName() (string, error) {
	cfg, _, err := c.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg.Smartnode.ProjectName.Value == "" {
		return "", errors.New("Rocket Pool docker project name not set")
	}
	return cfg.Smartnode.ProjectName.Value.(string) + APIContainerSuffix, nil
}

// Get gas price & limit flags
func (c *Client) getGasOpts() string {
	var opts string
	opts += fmt.Sprintf("--maxFee %f ", c.maxFee)
	opts += fmt.Sprintf("--maxPrioFee %f ", c.maxPrioFee)
	opts += fmt.Sprintf("--gasLimit %d ", c.gasLimit)
	return opts
}

func (c *Client) getCustomNonce() string {
	// Set the custom nonce
	nonce := ""
	if c.customNonce != nil {
		nonce = fmt.Sprintf("--nonce %s", c.customNonce.String())
	}
	return nonce
}

// Get the first downloader available to the system
func (c *Client) getDownloader() (string, error) {

	// Check for cURL
	hasCurl, err := c.readOutput("command -v curl")
	if err == nil && len(hasCurl) > 0 {
		return "curl -sL", nil
	}

	// Check for wget
	hasWget, err := c.readOutput("command -v wget")
	if err == nil && len(hasWget) > 0 {
		return "wget -qO-", nil
	}

	// Return error
	return "", errors.New("Either cURL or wget is required to begin installation.")

}

// pipeToStdOut pipes cmdOut to stdout
// Adds to WaitGroup and calls Done
func pipeToStdOut(cmdOut io.Reader, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	_, err := io.Copy(os.Stdout, cmdOut)
	if err != nil {
		log.Printf("Error piping stdout: %v", err)
	}
}

// pipeToStdErr pipes cmdErr to stderr
// Adds to WaitGroup and calls Done
func pipeToStdErr(cmdErr io.Reader, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	_, err := io.Copy(os.Stderr, cmdErr)
	if err != nil {
		log.Printf("Error piping stderr: %v", err)
	}
}

// pipeOutput pipes cmdOut and cmdErr to stdout and stderr
// Blocks until both cmdOut and cmdErr file descriptors are closed
func pipeOutput(cmdOut, cmdErr io.Reader) {
	var wg sync.WaitGroup

	go pipeToStdOut(cmdOut, &wg)
	go pipeToStdErr(cmdErr, &wg)

	wg.Wait()
}

// Run a command and print its output
func (c *Client) printOutput(cmdText string) error {

	// Initialize command
	cmd, err := c.newCommand(cmdText)
	if err != nil {
		return err
	}
	defer cmd.Close()

	cmdOut, cmdErr, err := cmd.OutputPipes()
	if err != nil {
		return err
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return err
	}

	pipeOutput(cmdOut, cmdErr)

	// Wait for the command to exit
	return cmd.Wait()

}

// Run a command and return its output
func (c *Client) readOutput(cmdText string) ([]byte, error) {

	// Initialize command
	cmd, err := c.newCommand(cmdText)
	if err != nil {
		return []byte{}, err
	}
	defer func() {
		_ = cmd.Close()
	}()

	// Run command and return output
	return cmd.Output()

}

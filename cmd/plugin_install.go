package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/codingsince1985/checksum"
	"github.com/gatewayd-io/gatewayd/config"
	"github.com/getsentry/sentry-go"
	"github.com/google/go-github/v53/github"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
)

const (
	NumParts                    int         = 2
	LatestVersion               string      = "latest"
	FolderPermissions           os.FileMode = 0o755
	DefaultPluginConfigFilename string      = "./gatewayd_plugin.yaml"
	GitHubURLPrefix             string      = "github.com/"
	GitHubURLRegex              string      = `^github.com\/[a-zA-Z0-9\-]+\/[a-zA-Z0-9\-]+@(?:latest|v(=|>=|<=|=>|=<|>|<|!=|~|~>|\^)?(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+(?P<buildmetadata>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?)$` //nolint:lll
	ExtWindows                  string      = ".zip"
	ExtOthers                   string      = ".tar.gz"
)

var (
	pluginOutputDir string
	pullOnly        bool
	cleanup         bool
	update          bool
	backupConfig    bool
	noPrompt        bool
)

// pluginInstallCmd represents the plugin install command.
var pluginInstallCmd = &cobra.Command{
	Use:     "install",
	Short:   "Install a plugin from a local archive or a GitHub repository",
	Example: "  gatewayd plugin install github.com/gatewayd-io/gatewayd-plugin-cache@latest",
	Run: func(cmd *cobra.Command, args []string) {
		// This is a list of files that will be deleted after the plugin is installed.
		toBeDeleted := []string{}

		// Enable Sentry.
		if enableSentry {
			// Initialize Sentry.
			err := sentry.Init(sentry.ClientOptions{
				Dsn:              DSN,
				TracesSampleRate: config.DefaultTraceSampleRate,
				AttachStacktrace: config.DefaultAttachStacktrace,
			})
			if err != nil {
				cmd.Println("Sentry initialization failed: ", err)
				return
			}

			// Flush buffered events before the program terminates.
			defer sentry.Flush(config.DefaultFlushTimeout)
			// Recover from panics and report the error to Sentry.
			defer sentry.Recover()
		}

		// Validate the number of arguments.
		if len(args) < 1 {
			cmd.Println(
				"Invalid URL. Use the following format: github.com/account/repository@version")
			return
		}

		var releaseID int64
		var downloadURL string
		var pluginFilename string
		var pluginName string
		var err error
		var checksumsFilename string
		var client *github.Client
		var account string

		// Strip scheme from the plugin URL.
		args[0] = strings.TrimPrefix(args[0], "http://")
		args[0] = strings.TrimPrefix(args[0], "https://")

		if !strings.HasPrefix(args[0], GitHubURLPrefix) {
			// Pull the plugin from a local archive.
			pluginFilename = filepath.Clean(args[0])
			if _, err := os.Stat(pluginFilename); os.IsNotExist(err) {
				cmd.Println("The plugin file could not be found")
				return
			}
		}

		// Validate the URL.
		validGitHubURL := regexp.MustCompile(GitHubURLRegex)
		if !validGitHubURL.MatchString(args[0]) {
			cmd.Println(
				"Invalid URL. Use the following format: github.com/account/repository@version")
			return
		}

		// Get the plugin version.
		pluginVersion := LatestVersion
		splittedURL := strings.Split(args[0], "@")
		// If the version is not specified, use the latest version.
		if len(splittedURL) < NumParts {
			cmd.Println("Version not specified. Using latest version")
		}
		if len(splittedURL) >= NumParts {
			pluginVersion = splittedURL[1]
		}

		// Get the plugin account and repository.
		accountRepo := strings.Split(strings.TrimPrefix(splittedURL[0], GitHubURLPrefix), "/")
		if len(accountRepo) != NumParts {
			cmd.Println(
				"Invalid URL. Use the following format: github.com/account/repository@version")
			return
		}
		account = accountRepo[0]
		pluginName = accountRepo[1]
		if account == "" || pluginName == "" {
			cmd.Println(
				"Invalid URL. Use the following format: github.com/account/repository@version")
			return
		}

		// Get the release artifact from GitHub.
		client = github.NewClient(nil)
		var release *github.RepositoryRelease

		if pluginVersion == LatestVersion || pluginVersion == "" {
			// Get the latest release.
			release, _, err = client.Repositories.GetLatestRelease(
				context.Background(), account, pluginName)
		} else if strings.HasPrefix(pluginVersion, "v") {
			// Get an specific release.
			release, _, err = client.Repositories.GetReleaseByTag(
				context.Background(), account, pluginName, pluginVersion)
		}

		if err != nil {
			cmd.Println("The plugin could not be found: ", err.Error())
			return
		}

		if release == nil {
			cmd.Println("The plugin could not be found in the release assets")
			return
		}

		// Get the archive extension.
		archiveExt := ExtOthers
		if runtime.GOOS == "windows" {
			archiveExt = ExtWindows
		}

		// Find and download the plugin binary from the release assets.
		pluginFilename, downloadURL, releaseID = findAsset(release, func(name string) bool {
			return strings.Contains(name, runtime.GOOS) &&
				strings.Contains(name, runtime.GOARCH) &&
				strings.Contains(name, archiveExt)
		})

		var filePath string
		if downloadURL != "" && releaseID != 0 {
			cmd.Println("Downloading", downloadURL)
			filePath, err = downloadFile(client, account, pluginName, releaseID, pluginFilename)
			toBeDeleted = append(toBeDeleted, filePath)
			if err != nil {
				cmd.Println("Download failed: ", err)
				if cleanup {
					deleteFiles(toBeDeleted)
				}
				return
			}
			cmd.Println("Download completed successfully")
		} else {
			cmd.Println("The plugin file could not be found in the release assets")
			return
		}

		// Find and download the checksums.txt from the release assets.
		checksumsFilename, downloadURL, releaseID = findAsset(release, func(name string) bool {
			return strings.Contains(name, "checksums.txt")
		})
		if checksumsFilename != "" && downloadURL != "" && releaseID != 0 {
			cmd.Println("Downloading", downloadURL)
			filePath, err = downloadFile(client, account, pluginName, releaseID, checksumsFilename)
			toBeDeleted = append(toBeDeleted, filePath)
			if err != nil {
				cmd.Println("Download failed: ", err)
				if cleanup {
					deleteFiles(toBeDeleted)
				}
				return
			}
			cmd.Println("Download completed successfully")
		} else {
			cmd.Println("The checksum file could not be found in the release assets")
			return
		}

		// Read the checksums text file.
		checksums, err := os.ReadFile(checksumsFilename)
		if err != nil {
			cmd.Println("There was an error reading the checksums file: ", err)
			return
		}

		// Get the checksum for the plugin binary.
		sum, err := checksum.SHA256sum(pluginFilename)
		if err != nil {
			cmd.Println("There was an error calculating the checksum: ", err)
			return
		}

		// Verify the checksums.
		checksumLines := strings.Split(string(checksums), "\n")
		for _, line := range checksumLines {
			if strings.Contains(line, pluginFilename) {
				checksum := strings.Split(line, " ")[0]
				if checksum != sum {
					cmd.Println("Checksum verification failed")
					return
				}

				cmd.Println("Checksum verification passed")
				break
			}
		}

		if pullOnly {
			cmd.Println("Plugin binary downloaded to", pluginFilename)
			// Only the checksums file will be deleted if the --pull-only flag is set.
			if err := os.Remove(checksumsFilename); err != nil {
				cmd.Println("There was an error deleting the file: ", err)
			}
			return
		}

		// Create a new gatewayd_plugins.yaml file if it doesn't exist.
		if _, err := os.Stat(pluginConfigFile); os.IsNotExist(err) {
			generateConfig(cmd, Plugins, pluginConfigFile, false)
		} else {
			// If the config file exists, we should prompt the user to backup
			// the plugins configuration file.
			if !backupConfig && !noPrompt {
				cmd.Print("Do you want to backup the plugins configuration file? [Y/n] ")
				var backupOption string
				_, err := fmt.Scanln(&backupOption)
				if err == nil && (backupOption == "y" || backupOption == "Y") {
					backupConfig = true
				}
			}
		}

		// Read the gatewayd_plugins.yaml file.
		pluginsConfig, err := os.ReadFile(pluginConfigFile)
		if err != nil {
			log.Println(err)
			return
		}

		// Get the registered plugins from the plugins configuration file.
		var localPluginsConfig map[string]interface{}
		if err := yamlv3.Unmarshal(pluginsConfig, &localPluginsConfig); err != nil {
			log.Println("Failed to unmarshal the plugins configuration file: ", err)
			return
		}
		pluginsList, ok := localPluginsConfig["plugins"].([]interface{}) //nolint:varnamelen
		if !ok {
			log.Println("There was an error reading the plugins file from disk")
			return
		}

		// Check if the plugin is already installed.
		for _, plugin := range pluginsList {
			// User already chosen to update the plugin using the --update CLI flag.
			if update {
				break
			}

			if pluginInstance, ok := plugin.(map[string]interface{}); ok {
				if pluginInstance["name"] == pluginName {
					// Show a list of options to the user.
					cmd.Println("Plugin is already installed.")
					if !noPrompt {
						cmd.Print("Do you want to update the plugin? [y/N] ")

						var updateOption string
						_, err := fmt.Scanln(&updateOption)
						if err == nil && (updateOption == "y" || updateOption == "Y") {
							break
						}
					}

					cmd.Println("Aborting...")
					if cleanup {
						deleteFiles(toBeDeleted)
					}
					return
				}
			}
		}

		// Check if the user wants to take a backup of the plugins configuration file.
		if backupConfig {
			backupFilename := fmt.Sprintf("%s.bak", pluginConfigFile)
			if err := os.WriteFile(backupFilename, pluginsConfig, FilePermissions); err != nil {
				cmd.Println("There was an error backing up the plugins configuration file: ", err)
			}
			cmd.Println("Backup completed successfully")
		}

		// Extract the archive.
		var filenames []string
		if runtime.GOOS == "windows" {
			filenames, err = extractZip(pluginFilename, pluginOutputDir)
		} else {
			filenames, err = extractTarGz(pluginFilename, pluginOutputDir)
		}

		if err != nil {
			cmd.Println("There was an error extracting the plugin archive: ", err)
			if cleanup {
				deleteFiles(toBeDeleted)
			}
			return
		}

		// Delete all the files except the extracted plugin binary,
		// which will be deleted from the list further down.
		toBeDeleted = append(toBeDeleted, filenames...)

		// Find the extracted plugin binary.
		localPath := ""
		pluginFileSum := ""
		for _, filename := range filenames {
			if strings.Contains(filename, pluginName) {
				cmd.Println("Plugin binary extracted to", filename)

				// Remove the plugin binary from the list of files to be deleted.
				toBeDeleted = slices.DeleteFunc[[]string, string](toBeDeleted, func(s string) bool {
					return s == filename
				})

				localPath = filename
				// Get the checksum for the extracted plugin binary.
				// TODO: Should we verify the checksum using the checksum.txt file instead?
				pluginFileSum, err = checksum.SHA256sum(filename)
				if err != nil {
					cmd.Println("There was an error calculating the checksum: ", err)
					return
				}
				break
			}
		}

		var contents string
		if strings.HasPrefix(args[0], GitHubURLPrefix) {
			// Get the list of files in the repository.
			var repoContents *github.RepositoryContent
			repoContents, _, _, err = client.Repositories.GetContents(
				context.Background(), account, pluginName, DefaultPluginConfigFilename, nil)
			if err != nil {
				cmd.Println(
					"There was an error getting the default plugins configuration file: ", err)
				return
			}
			// Get the contents of the file.
			contents, err = repoContents.GetContent()
			if err != nil {
				cmd.Println(
					"There was an error getting the default plugins configuration file: ", err)
				return
			}
		} else {
			// Get the contents of the file.
			contentsBytes, err := os.ReadFile(
				filepath.Join(pluginOutputDir, DefaultPluginConfigFilename))
			if err != nil {
				cmd.Println(
					"There was an error getting the default plugins configuration file: ", err)
				return
			}
			contents = string(contentsBytes)
		}

		// Get the plugin configuration from the downloaded plugins configuration file.
		var downloadedPluginConfig map[string]interface{}
		if err := yamlv3.Unmarshal([]byte(contents), &downloadedPluginConfig); err != nil {
			cmd.Println("Failed to unmarshal the downloaded plugins configuration file: ", err)
			return
		}
		defaultPluginConfig, ok := downloadedPluginConfig["plugins"].([]interface{})
		if !ok {
			cmd.Println("There was an error reading the plugins file from the repository")
			return
		}
		// Get the plugin configuration.
		pluginConfig, ok := defaultPluginConfig[0].(map[string]interface{})
		if !ok {
			cmd.Println("There was an error reading the default plugin configuration")
			return
		}

		// Update the plugin's local path and checksum.
		pluginConfig["localPath"] = localPath
		pluginConfig["checksum"] = pluginFileSum

		// Add the plugin config to the list of plugin configs.
		added := false
		for idx, plugin := range pluginsList {
			if pluginInstance, ok := plugin.(map[string]interface{}); ok {
				if pluginInstance["name"] == pluginName {
					pluginsList[idx] = pluginConfig
					added = true
					break
				}
			}
		}
		if !added {
			pluginsList = append(pluginsList, pluginConfig)
		}

		// Merge the result back into the config map.
		localPluginsConfig["plugins"] = pluginsList

		// Marshal the map into YAML.
		updatedPlugins, err := yamlv3.Marshal(localPluginsConfig)
		if err != nil {
			cmd.Println("There was an error marshalling the plugins configuration: ", err)
			return
		}

		// Write the YAML to the plugins config file.
		if err = os.WriteFile(pluginConfigFile, updatedPlugins, FilePermissions); err != nil {
			cmd.Println("There was an error writing the plugins configuration file: ", err)
			return
		}

		// Delete the downloaded and extracted files, except the plugin binary,
		// if the --cleanup flag is set.
		if cleanup {
			deleteFiles(toBeDeleted)
		}

		// TODO: Add a rollback mechanism.
		cmd.Println("Plugin installed successfully")
	},
}

func init() {
	pluginCmd.AddCommand(pluginInstallCmd)

	pluginInstallCmd.Flags().StringVarP(
		&pluginConfigFile, // Already exists in run.go
		"plugin-config", "p", config.GetDefaultConfigFilePath(config.PluginsConfigFilename),
		"Plugin config file")
	pluginInstallCmd.Flags().StringVarP(
		&pluginOutputDir, "output-dir", "o", "./plugins", "Output directory for the plugin")
	pluginInstallCmd.Flags().BoolVar(
		&pullOnly, "pull-only", false, "Only pull the plugin, don't install it")
	pluginInstallCmd.Flags().BoolVar(
		&cleanup, "cleanup", true,
		"Delete downloaded and extracted files after installing the plugin (except the plugin binary)")
	pluginInstallCmd.Flags().BoolVar(
		&noPrompt, "no-prompt", true, "Do not prompt for user input")
	pluginInstallCmd.Flags().BoolVar(
		&update, "update", false, "Update the plugin if it already exists")
	pluginInstallCmd.Flags().BoolVar(
		&backupConfig, "backup", false, "Backup the plugins configuration file before installing the plugin")
	pluginInstallCmd.Flags().BoolVar(
		&enableSentry, "sentry", true, "Enable Sentry") // Already exists in run.go
}

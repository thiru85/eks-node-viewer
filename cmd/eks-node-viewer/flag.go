/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/client-go/util/homedir"
)

var (
	homeDir    string
	configPath string
	version    = "dev"
	commit     = ""
	date       = ""
	builtBy    = ""
)

func init() {
	homeDir = homedir.HomeDir()
	configPath = filepath.Join(homeDir, ".eks-node-viewer")
}

type Flags struct {
	Context         string
	NodeSelector    string
	ExtraLabels     string
	Kubeconfig      string
	Resources       string
	DisablePricing  bool
	ShowAttribution bool
	Version         bool
	InvertColors    bool
}

func ParseFlags() (Flags, error) {
	flagSet := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var flags Flags

	cfg, err := loadConfigFile()
	if err != nil {
		return Flags{}, fmt.Errorf("load config file: %w", err)
	}

	flagSet.BoolVar(&flags.Version, "v", false, "Display eks-node-viewer version")
	flagSet.BoolVar(&flags.Version, "version", false, "Display eks-node-viewer version")

	contextDefault := cfg.getValue("context", "")
	flagSet.StringVar(&flags.Context, "context", contextDefault, "Name of the kubernetes context to use")

	nodeSelectorDefault := cfg.getValue("node-selector", "")
	flagSet.StringVar(&flags.NodeSelector, "node-selector", nodeSelectorDefault, "Node label selector used to filter nodes, if empty all nodes are selected ")

	extraLabelsDefault := cfg.getValue("extra-labels", "")
	flagSet.StringVar(&flags.ExtraLabels, "extra-labels", extraLabelsDefault, "A comma separated set of extra node labels to display")

	// flag overrides env. var. and env. var. overrides config file
	kubeconfigDefault := getStringEnv("KUBECONFIG", cfg.getValue("kubeconfig", filepath.Join(homeDir, ".kube", "config")))
	flagSet.StringVar(&flags.Kubeconfig, "kubeconfig", kubeconfigDefault, "Absolute path to the kubeconfig file")

	resourcesDefault := cfg.getValue("resources", "cpu")
	flagSet.StringVar(&flags.Resources, "resources", resourcesDefault, "List of comma separated resources to monitor")

	disablePricingDefault := cfg.getBoolValue("disable-pricing", false)
	flagSet.BoolVar(&flags.DisablePricing, "disable-pricing", disablePricingDefault, "Disable pricing lookups")

	flagSet.BoolVar(&flags.ShowAttribution, "attribution", false, "Show the Open Source Attribution")

	invertColorsDefault := cfg.getBoolValue("invert-colors", false)
	flagSet.BoolVar(&flags.InvertColors, "invert-colors", invertColorsDefault, "Invert colors of the utilization bar")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		return Flags{}, err
	}
	return flags, nil
}

// --- env vars ---

func getStringEnv(envName string, defaultValue string) string {
	env, ok := os.LookupEnv(envName)
	if !ok {
		return defaultValue
	}
	return env
}

// --- config file ---

type configFile map[string]string

func (c configFile) getValue(key string, defaultValue string) string {
	if val, ok := c[key]; ok {
		return val
	}
	return defaultValue
}

func (c configFile) getBoolValue(key string, defaultValue bool) bool {
	if val, ok := c[key]; ok {
		if boolVal, err := strconv.ParseBool(val); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func loadConfigFile() (configFile, error) {
	fileContent := make(map[string]string)
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return fileContent, nil
	}

	file, err := os.Open(configPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		lineKV := strings.SplitN(line, "=", 2)
		if len(lineKV) == 2 {
			key := strings.TrimSpace(lineKV[0])
			value := strings.TrimSpace(lineKV[1])
			fileContent[key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return fileContent, nil
}

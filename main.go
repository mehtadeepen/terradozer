package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/plugin/discovery"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/states/statefile"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/sirupsen/logrus"
	"github.com/zclconf/go-cty/cty"
)

type TerraformProvider struct {
	providers.Interface
}

func main() {
	profile := "tfsweeper"
	region := "us-west-2"

	logrus.SetFormatter(&logrus.TextFormatter{
		DisableTimestamp: true,
	})
	logrus.SetLevel(logrus.DebugLevel)

	p, err := loadAWSProvider()
	if err != nil {
		logrus.WithError(err).Fatal("failed to load Terraform AWS resource provider")
	}

	tfDiagnostics := p.configure(profile, region)
	if tfDiagnostics.HasErrors() {
		logrus.WithError(tfDiagnostics.Err()).Fatal("failed to configure Terraform provider")
	}

	state, err := getState()
	if err != nil {
		logrus.WithError(err).Fatal("failed to read tfstate from local file")
	}

	resInstances, diagnostics := lookupAllResourceInstanceAddrs(state)
	if diagnostics.HasErrors() {
		logrus.WithError(diagnostics.Err()).Fatal("failed to lookup resource instance addresses")
	}

	for _, resAddr := range resInstances {
		if resInstance := state.ResourceInstance(resAddr); resInstance.HasCurrent() {
			resMode := resAddr.Resource.Resource.Mode
			resID := resInstance.Current.AttrsFlat["id"]
			resType := resAddr.Resource.Resource.Type

			if resMode == addrs.ManagedResourceMode {
				logrus.WithFields(map[string]interface{}{
					"id": resID,
				}).Print(resAddr.String())

				resImported, tfDiagnostics := p.importResource(resType, resID)
				if tfDiagnostics.HasErrors() {
					logrus.WithError(tfDiagnostics.Err()).Infof("failed to import resource (type=%s, id=%s)", resType, resID)
					continue
				}

				for _, r := range resImported {
					logrus.Debugf("imported resource (type=%s, id=%s): %s", r.TypeName, resID, r.State.GoString())
				}
			}
		}
	}
}

func loadAWSProvider() (*TerraformProvider, error) {
	awsProviderPluginData := discovery.PluginMeta{
		Name:    "terraform-provider-aws",
		Version: "v2.33.0",
		Path:    "./terraform-provider-aws_v2.33.0_x4",
	}

	awsProvider, err := providerFactory(awsProviderPluginData)()
	if err != nil {
		return nil, err
	}
	return &TerraformProvider{awsProvider}, nil
}

// copied from github.com/hashicorp/terraform/command/plugins.go
func providerFactory(meta discovery.PluginMeta) providers.Factory {
	return func() (providers.Interface, error) {
		client := plugin.Client(meta)
		// Request the RPC client so we can get the provider
		// so we can build the actual RPC-implemented provider.
		rpcClient, err := client.Client()
		if err != nil {
			return nil, err
		}

		raw, err := rpcClient.Dispense(plugin.ProviderPluginName)
		if err != nil {
			return nil, err
		}

		// store the client so that the plugin can kill the child process
		p := raw.(*plugin.GRPCProvider)
		p.PluginClient = client
		return p, nil
	}
}

func getState() (*states.State, error) {
	stateFile, err := getStateFromPath("terraform.tfstate")
	if err != nil {
		return nil, err
	}
	return stateFile.State, nil
}

func (p TerraformProvider) configure(profile, region string) tfdiags.Diagnostics {
	respConf := p.Configure(providers.ConfigureRequest{
		TerraformVersion: "0.12.11",
		Config: cty.ObjectVal(map[string]cty.Value{
			"profile":                     cty.StringVal(profile),
			"region":                      cty.StringVal(region),
			"access_key":                  cty.UnknownVal(cty.DynamicPseudoType),
			"allowed_account_ids":         cty.UnknownVal(cty.DynamicPseudoType),
			"assume_role":                 cty.UnknownVal(cty.DynamicPseudoType),
			"endpoints":                   cty.UnknownVal(cty.DynamicPseudoType),
			"forbidden_account_ids":       cty.UnknownVal(cty.DynamicPseudoType),
			"insecure":                    cty.UnknownVal(cty.DynamicPseudoType),
			"max_retries":                 cty.UnknownVal(cty.DynamicPseudoType),
			"s3_force_path_style":         cty.UnknownVal(cty.DynamicPseudoType),
			"secret_key":                  cty.UnknownVal(cty.DynamicPseudoType),
			"shared_credentials_file":     cty.UnknownVal(cty.DynamicPseudoType),
			"skip_credentials_validation": cty.UnknownVal(cty.DynamicPseudoType),
			"skip_get_ec2_platforms":      cty.UnknownVal(cty.DynamicPseudoType),
			"skip_metadata_api_check":     cty.UnknownVal(cty.DynamicPseudoType),
			"skip_region_validation":      cty.UnknownVal(cty.DynamicPseudoType),
			"skip_requesting_account_id":  cty.UnknownVal(cty.DynamicPseudoType),
			"token":                       cty.UnknownVal(cty.DynamicPseudoType),
		})})

	return respConf.Diagnostics
}

func (p TerraformProvider) importResource(resType string, resID string) ([]providers.ImportedResource, tfdiags.Diagnostics) {
	respImport := p.ImportResourceState(providers.ImportResourceStateRequest{
		TypeName: resType,
		ID:       resID,
	})

	return respImport.ImportedResources, respImport.Diagnostics
}

// copied from github.com/hashicorp/terraform/command/show.go
func getStateFromPath(path string) (*statefile.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed loading statefile: %s", err)
	}
	defer f.Close()

	var stateFile *statefile.File
	stateFile, err = statefile.Read(f)
	if err != nil {
		return nil, fmt.Errorf("failed reading %s as a statefile: %s", path, err)
	}
	return stateFile, nil
}

// copied from github.com/hashicorp/terraform/command/state_meta.go
func lookupAllResourceInstanceAddrs(state *states.State) ([]addrs.AbsResourceInstance, tfdiags.Diagnostics) {
	var ret []addrs.AbsResourceInstance
	var diags tfdiags.Diagnostics
	for _, ms := range state.Modules {
		ret = append(ret, collectModuleResourceInstances(ms)...)
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].Less(ret[j])
	})
	return ret, diags
}

// copied from github.com/hashicorp/terraform/command/state_meta.go
func collectModuleResourceInstances(ms *states.Module) []addrs.AbsResourceInstance {
	var ret []addrs.AbsResourceInstance
	for _, rs := range ms.Resources {
		ret = append(ret, collectResourceInstances(ms.Addr, rs)...)
	}
	return ret
}

// copied from github.com/hashicorp/terraform/command/state_meta.go
func collectResourceInstances(moduleAddr addrs.ModuleInstance, rs *states.Resource) []addrs.AbsResourceInstance {
	var ret []addrs.AbsResourceInstance
	for key := range rs.Instances {
		ret = append(ret, rs.Addr.Instance(key).Absolute(moduleAddr))
	}
	return ret
}

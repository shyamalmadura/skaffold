/*
Copyright 2019 The Skaffold Authors

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

package kubectl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/config"
	deployerr "github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/error"
	deploy "github.com/GoogleContainerTools/skaffold/pkg/skaffold/deploy/types"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/manifest"
	latestV1 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest/v1"
)

// CLI holds parameters to run kubectl.
type CLI struct {
	*kubectl.CLI
	Flags latestV1.KubectlFlags

	forceDeploy      bool
	waitForDeletions config.WaitForDeletions
	previousApply    manifest.ManifestList
}

type Config interface {
	kubectl.Config
	deploy.Config
	ForceDeploy() bool
	WaitForDeletions() config.WaitForDeletions
	Mode() config.RunMode
	HydratedManifests() []string
}

func NewCLI(cfg Config, flags latestV1.KubectlFlags, defaultNameSpace string) CLI {
	return CLI{
		CLI:              kubectl.NewCLI(cfg, defaultNameSpace),
		Flags:            flags,
		forceDeploy:      cfg.ForceDeploy(),
		waitForDeletions: cfg.WaitForDeletions(),
	}
}

// Delete runs `kubectl delete` on a list of manifests.
func (c *CLI) Delete(ctx context.Context, out io.Writer, manifests manifest.ManifestList) error {
	args := c.args(c.Flags.Delete, "--ignore-not-found=true", "-f", "-")
	if err := c.Run(ctx, manifests.Reader(), out, "delete", args...); err != nil {
		return deployerr.CleanupErr(fmt.Errorf("kubectl delete: %w", err))
	}

	return nil
}

// Apply runs `kubectl apply` on a list of manifests.
func (c *CLI) Apply(ctx context.Context, out io.Writer, manifests manifest.ManifestList) error {
	// Only redeploy modified or new manifests
	// TODO(dgageot): should we delete a manifest that was deployed and is not anymore?
	updated := c.previousApply.Diff(manifests)
	logrus.Debugln(len(manifests), "manifests to deploy.", len(updated), "are updated or new")
	c.previousApply = manifests
	if len(updated) == 0 {
		return nil
	}

	args := []string{"-f", "-"}
	if c.forceDeploy {
		args = append(args, "--force", "--grace-period=0")
	}

	if c.Flags.DisableValidation {
		args = append(args, "--validate=false")
	}

	if err := c.Run(ctx, updated.Reader(), out, "apply", c.args(c.Flags.Apply, args...)...); err != nil {
		return userErr(fmt.Errorf("kubectl apply: %w", err))
	}

	return nil
}

// Kustomize runs `kubectl kustomize` with the provided args
func (c *CLI) Kustomize(ctx context.Context, args []string) ([]byte, error) {
	return c.RunOut(ctx, "kustomize", c.args(nil, args...)...)
}

type getResult struct {
	Items []struct {
		Metadata struct {
			Name              string `json:"name"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
	} `json:"items"`
}

// WaitForDeletions waits for resource marked for deletion to complete their deletion.
func (c *CLI) WaitForDeletions(ctx context.Context, out io.Writer, manifests manifest.ManifestList) error {
	if !c.waitForDeletions.Enabled {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.waitForDeletions.Max)
	defer cancel()

	previousList := ""
	previousCount := 0

	for {
		select {
		case <-ctx.Done():
			return waitForDeletionErr(fmt.Errorf("%d resources failed to complete their deletion before a new deployment: %s", previousCount, previousList))
		default:
			// List resources in json format.
			buf, err := c.RunOutInput(ctx, manifests.Reader(), "get", c.args(nil, "-f", "-", "--ignore-not-found", "-ojson")...)
			if err != nil {
				return waitForDeletionErr(err)
			}

			// No resource found.
			if len(buf) == 0 {
				return nil
			}

			// Find which ones are marked for deletion. They have a `metadata.deletionTimestamp` field.
			var result getResult
			if err := json.Unmarshal(buf, &result); err != nil {
				return waitForDeletionErr(err)
			}

			var marked []string
			for _, item := range result.Items {
				if item.Metadata.DeletionTimestamp != "" {
					marked = append(marked, item.Metadata.Name)
				}
			}
			if len(marked) == 0 {
				return nil
			}

			list := `"` + strings.Join(marked, `", "`) + `"`
			logrus.Debugln("Resources are marked for deletion:", list)
			if list != previousList {
				if len(marked) == 1 {
					fmt.Fprintf(out, "%s is marked for deletion, waiting for completion\n", list)
				} else {
					fmt.Fprintf(out, "%d resources are marked for deletion, waiting for completion: %s\n", len(marked), list)
				}

				previousList = list
				previousCount = len(marked)
			}

			select {
			case <-ctx.Done():
			case <-time.After(c.waitForDeletions.Delay):
			}
		}
	}
}

// ReadManifests reads a list of manifests in yaml format.
func (c *CLI) ReadManifests(ctx context.Context, manifests []string) (manifest.ManifestList, error) {
	var list []string
	for _, manifest := range manifests {
		list = append(list, "-f", manifest)
	}

	var dryRun = "--dry-run"
	compTo1_18, err := c.CLI.CompareVersionTo(ctx, 1, 18)
	if err != nil {
		return nil, versionGetErr(err)
	}
	if compTo1_18 >= 0 {
		dryRun += "=client"
	}

	args := c.args([]string{dryRun, "-oyaml"}, list...)
	if c.Flags.DisableValidation {
		args = append(args, "--validate=false")
	}

	buf, err := c.RunOut(ctx, "create", args...)
	if err != nil {
		return nil, readManifestErr(fmt.Errorf("kubectl create: %w", err))
	}

	var manifestList manifest.ManifestList
	manifestList.Append(buf)

	return manifestList, nil
}

func (c *CLI) args(commandFlags []string, additionalArgs ...string) []string {
	args := make([]string, 0, len(c.Flags.Global)+len(commandFlags)+len(additionalArgs))

	args = append(args, c.Flags.Global...)
	args = append(args, commandFlags...)
	args = append(args, additionalArgs...)

	return args
}

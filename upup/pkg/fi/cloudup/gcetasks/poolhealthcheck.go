/*
Copyright 2022 The Kubernetes Authors.

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

package gcetasks

import (
	"fmt"

	"slices"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/gce"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// PoolHealthCheck represents a GCE target pool HealthCheck
// +kops:fitask
type PoolHealthCheck struct {
	Name        *string
	Lifecycle   fi.Lifecycle
	Healthcheck *HTTPHealthcheck
	Pool        *TargetPool
}

var _ fi.CompareWithID = &PoolHealthCheck{}

// GetDependencies returns the dependencies of the PoolHealthCheck task
func (_ *PoolHealthCheck) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	var deps []fi.CloudupTask
	for _, task := range tasks {
		if _, ok := task.(*HTTPHealthcheck); ok {
			deps = append(deps, task)
		}
		if _, ok := task.(*TargetPool); ok {
			deps = append(deps, task)
		}
	}
	return deps
}

func (e *PoolHealthCheck) CompareWithID() *string {
	return e.Name
}

func (e *PoolHealthCheck) Find(c *fi.CloudupContext) (*PoolHealthCheck, error) {
	cloud := c.T.Cloud.(gce.GCECloud)
	name := fi.ValueOf(e.Pool.Name)
	r, err := cloud.Compute().TargetPools().Get(cloud.Project(), cloud.Region(), name)
	if err != nil {
		if gce.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error getting TargetPool %q: %v", name, err)
	}
	if slices.Contains(r.HealthChecks, e.Healthcheck.SelfLink) {
		return &PoolHealthCheck{
			Name:        e.Name,
			Healthcheck: e.Healthcheck,
			Pool:        e.Pool,
			Lifecycle:   e.Lifecycle,
		}, nil
	}
	return nil, nil
}

func (e *PoolHealthCheck) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

func (_ *PoolHealthCheck) CheckChanges(a, e, changes *PoolHealthCheck) error {
	return nil
}

func (p *PoolHealthCheck) RenderGCE(t *gce.GCEAPITarget, a, e, changes *PoolHealthCheck) error {
	if a == nil {
		targetPool := fi.ValueOf(p.Pool.Name)
		req := &compute.TargetPoolsAddHealthCheckRequest{
			HealthChecks: []*compute.HealthCheckReference{
				{
					HealthCheck: p.Healthcheck.SelfLink,
				},
			},
		}
		op, err := t.Cloud.Compute().TargetPools().AddHealthCheck(t.Cloud.Project(), t.Cloud.Region(), targetPool, req)
		if err != nil {
			return fmt.Errorf("error creating PoolHealthCheck: %v", err)
		}

		if err := t.Cloud.WaitForOp(op); err != nil {
			return fmt.Errorf("error creating PoolHealthCheck: %v", err)
		}
	}
	return nil
}

func (_ *PoolHealthCheck) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *PoolHealthCheck) error {
	return nil
}
